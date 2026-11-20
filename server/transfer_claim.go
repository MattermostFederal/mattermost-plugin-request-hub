package main

import (
	"strconv"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

// Atomic per-asset lock for bot/webhook transfer tickets. Soft duplicate
// scans (transferAssetPendingElsewhere) race under concurrent submit; this
// CAS claim is the source of truth for "one open ticket per asset".
//
// One KV value owns the asset for the whole life of a ticket. It carries
// the holding request, a generation, the phase that request is in, when its
// lease was last renewed, and which fulfill attempt wrote it:
//
//	requestID|generation|phase|leaseAt|attemptID
//
// Submit stores the request record before claiming, so pendingRequestExists
// is true for the holder during the claim window. Final approval deletes
// the record before the REST side effects run, so the claim moves to the
// fulfilling phase first and the asset is never unprotected while the
// record is gone. Long side effects renew the lease as they go; a crash
// frees the asset once the lease ages out instead of locking it for good.
//
// Putting a record back after a failed fulfill runs in the restoring phase,
// which no takeover may break while its lease is alive: the gap between
// "I still hold the claim" and the KV write is exactly where two tickets
// could end up on one asset. Entering restoring renews the lease, so a
// restoring claim whose lease has aged out belongs to a restorer that died
// mid-write and is treated as abandoned — otherwise the asset would be
// locked for good.

const (
	kvTransferAssetClaimPrefix = "transfer_asset_claim_"

	// Claim phases. An empty phase (values written before phases existed)
	// reads as held.
	transferPhaseHeld       = "held"
	transferPhaseFulfilling = "fulfilling"
	transferPhaseRestoring  = "restoring"

	// transferFulfillWindow bounds how long a lease protects a claim
	// without a renewal. Revoke-all walks every token on a bot, each call
	// capped by transferRESTTimeout, so the window is generous and the
	// lease is renewed as the loop runs (renewTransferLease).
	transferFulfillWindow = 60 * time.Minute

	transferClaimCASRetries = 5

	// transferRecordPutRetries bounds how many times a restore re-tries the
	// write that puts a claimed ticket back.
	transferRecordPutRetries = 3
)

// errTransferClaimLost reports that another ticket owns the asset claim.
var errTransferClaimLost = errors.New("another transfer ticket took over the asset claim")

// errTransferFulfillInFlight reports that this ticket's approval is already
// being carried out under a live lease, so a second approver must not start
// a parallel run of the same side effects.
var errTransferFulfillInFlight = errors.New("this transfer is already being carried out")

// transferClaim is a decoded per-asset claim value.
type transferClaim struct {
	requestID string
	// generation rises each time a holder re-publishes its record, so a
	// takeover that already read the claim loses its CAS instead of
	// stealing a live ticket.
	generation int64
	phase      string
	leaseAt    int64 // millis of the last renewal
	// attemptID separates two fulfill attempts on the same ticket that land
	// in the same millisecond, so an attempt that has to take its own row
	// back can never CAS-delete the row another attempt owns.
	attemptID string
}

// transferLease names the claim a long-running fulfill has to keep
// renewing. The zero value turns renewal off (non-transfer callers).
type transferLease struct {
	kind      string
	assetKey  string
	requestID string
	// attemptID pins the lease to one fulfill attempt, so an approval that
	// paused and came back cannot renew, release or act under the lease a
	// newer attempt took over.
	attemptID string
}

func (l transferLease) active() bool {
	return l.kind != "" && l.assetKey != "" && l.requestID != ""
}

// ownedBy reports whether c is the claim requestID/attemptID were taken on.
// An attempt only ever loses to another attempt: a claim carrying no
// attempt (a plain pending ticket) still belongs to its holder.
func (c transferClaim) ownedBy(requestID, attemptID string) bool {
	if c.requestID != requestID {
		return false
	}
	return attemptID == "" || c.attemptID == "" || c.attemptID == attemptID
}

func (c transferClaim) encode() []byte {
	phase := c.phase
	if phase == "" {
		phase = transferPhaseHeld
	}
	return []byte(strings.Join([]string{
		c.requestID,
		strconv.FormatInt(c.generation, 10),
		phase,
		strconv.FormatInt(c.leaseAt, 10),
		c.attemptID,
	}, "|"))
}

// leaseExpired reports whether the holder's lease has aged out. Values
// written before leases existed carry no timestamp and read as expired.
func (c transferClaim) leaseExpired(now int64) bool {
	return now-c.leaseAt >= transferFulfillWindow.Milliseconds()
}

// parseTransferClaim reads a claim value. Bare request IDs and the older
// "requestID|generation" form are still understood.
func parseTransferClaim(raw []byte) transferClaim {
	parts := strings.Split(strings.TrimSpace(string(raw)), "|")
	claim := transferClaim{requestID: strings.TrimSpace(parts[0]), phase: transferPhaseHeld}
	if len(parts) > 1 {
		claim.generation, _ = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	}
	if len(parts) > 2 {
		if phase := strings.TrimSpace(parts[2]); phase != "" {
			claim.phase = phase
		}
	}
	if len(parts) > 3 {
		claim.leaseAt, _ = strconv.ParseInt(strings.TrimSpace(parts[3]), 10, 64)
	}
	if len(parts) > 4 {
		claim.attemptID = strings.TrimSpace(parts[4])
	}
	return claim
}

func transferAssetClaimKey(kind, assetKey string) string {
	return kvTransferAssetClaimPrefix + kind + "_" + assetKey
}

// transferPendingAssetKey returns the pendingKey for a transfer record, or
// empty when rec is not a bot/webhook transfer.
func transferPendingAssetKey(rec storedRequest) string {
	switch r := rec.(type) {
	case *botTransferRequest:
		return r.pendingKey()
	case *webhookTransferRequest:
		return r.pendingKey()
	default:
		return ""
	}
}

// claimTransferAsset CAS-writes requestID as the holder of assetKey. If
// another live request already holds the claim (or another open ticket
// covers the asset), returns a duplicate error. A claim is only taken over
// when its holder's record is gone, it is not mid-restore, its lease has
// aged out, and no other open ticket covers the asset.
func (p *Plugin) claimTransferAsset(kind, assetKey, requestID string) error {
	if kind != pendingKindBotTransfer && kind != pendingKindWebhookTransfer {
		return nil
	}
	assetKey = strings.TrimSpace(assetKey)
	requestID = strings.TrimSpace(requestID)
	if assetKey == "" || requestID == "" {
		return errors.New("could not claim that asset; try again")
	}
	key := transferAssetClaimKey(kind, assetKey)
	fresh := transferClaim{requestID: requestID, phase: transferPhaseHeld, leaseAt: model.GetMillis()}

	ok, appErr := p.API.KVCompareAndSet(key, nil, fresh.encode())
	if appErr != nil {
		return errors.New("could not claim that asset; try again")
	}
	if ok {
		return nil
	}

	raw, getErr := p.API.KVGet(key)
	if getErr != nil {
		return errors.New("could not claim that asset; try again")
	}
	claim := parseTransferClaim(raw)
	if claim.requestID == requestID {
		return nil
	}
	if claim.requestID != "" {
		exists, err := p.pendingRequestExists(kind, claim.requestID)
		if err != nil {
			return errors.New("could not check for an existing transfer ticket; try again")
		}
		if exists {
			return duplicateRequestError(kind, assetKey)
		}
		// The holder's record is missing: it is between the fulfill claim
		// and its REST calls, or putting the record back. Either way it is
		// protected until its lease runs out — including a restoring
		// holder, which renews the lease as it republishes.
		if !claim.leaseExpired(model.GetMillis()) {
			return duplicateRequestError(kind, assetKey)
		}
		held, holdErr := p.transferAssetPendingElsewhere(kind, assetKey, requestID)
		if holdErr != nil {
			return errors.New("could not check for an existing transfer ticket; try again")
		}
		if held {
			return duplicateRequestError(kind, assetKey)
		}
	}

	ok, appErr = p.API.KVCompareAndSet(key, raw, fresh.encode())
	if appErr != nil {
		return errors.New("could not claim that asset; try again")
	}
	if !ok {
		return duplicateRequestError(kind, assetKey)
	}
	return nil
}

// updateTransferClaim CAS-applies fn to the claim while requestID — and,
// when both sides carry one, attemptID — still holds it, retrying when
// another write lands first. fn runs against the exact value about to be
// replaced on every pass, including after a lost CAS, and may refuse the
// change by returning an error. errTransferClaimLost means the asset moved
// on and the caller must stop before its next side effect.
func (p *Plugin) updateTransferClaim(kind, assetKey, requestID, attemptID string, fn func(*transferClaim) error) error {
	key := transferAssetClaimKey(kind, assetKey)
	for range transferClaimCASRetries {
		raw, appErr := p.API.KVGet(key)
		if appErr != nil {
			return errors.Wrap(appErr, "failed to read transfer asset claim")
		}
		claim := parseTransferClaim(raw)
		if !claim.ownedBy(requestID, attemptID) {
			return errTransferClaimLost
		}
		next := claim
		if err := fn(&next); err != nil {
			return err
		}
		ok, casErr := p.API.KVCompareAndSet(key, raw, next.encode())
		if casErr != nil {
			return errors.Wrap(casErr, "failed to update transfer asset claim")
		}
		if ok {
			return nil
		}
	}
	return errors.New("failed to update transfer asset claim after concurrent updates")
}

// beginTransferFulfill moves the claim into the fulfilling phase on a fresh
// lease. It runs before the pending record is deleted, so the asset stays
// protected while the REST side effects run.
//
// created is true when this call invented the claim row from nothing, and
// claimBytes are the exact bytes it wrote — an approval that then fails to
// claim the record has to take its own row back rather than release a claim
// it never really owned. Every attempt carries its own attemptID, so two
// approvers entering in the same millisecond never write the same bytes.
//
// errTransferFulfillInFlight means another approver is already running this
// ticket's side effects under a live lease; the checkpoint it leaves behind
// is what lets a retry pick the work up once that lease ages out.
func (p *Plugin) beginTransferFulfill(kind, assetKey, requestID string) (created bool, claimBytes []byte, err error) {
	// A ticket filed before per-asset claims existed has no claim row, and
	// an unclaimed asset belongs to nobody: take it rather than refuse the
	// approval.
	key := transferAssetClaimKey(kind, assetKey)
	fresh := transferClaim{
		requestID: requestID, phase: transferPhaseFulfilling,
		leaseAt: model.GetMillis(), attemptID: model.NewId(),
	}.encode()
	ok, appErr := p.API.KVCompareAndSet(key, nil, fresh)
	if appErr != nil {
		return false, nil, errors.Wrap(appErr, "failed to update transfer asset claim")
	}
	if ok {
		return true, fresh, nil
	}
	var written []byte
	err = p.updateTransferClaim(kind, assetKey, requestID, "", func(c *transferClaim) error {
		// Checked against the exact value being replaced rather than a
		// value read earlier: two approvers can both read the same held
		// claim, and the second must not overwrite the attempt the first
		// one already started. A held claim, or an attempt whose lease has
		// aged out, is free to take.
		//
		// A live restoring claim is just as off-limits: that holder is
		// between "I own this" and the KV write that puts its record back,
		// and a takeover there is exactly how one asset ends up with two
		// open tickets.
		switch c.phase {
		case transferPhaseFulfilling, transferPhaseRestoring:
			if !c.leaseExpired(model.GetMillis()) {
				return errTransferFulfillInFlight
			}
		}
		c.phase = transferPhaseFulfilling
		c.leaseAt = model.GetMillis()
		c.attemptID = model.NewId()
		written = c.encode()
		return nil
	})
	if err != nil {
		return false, nil, err
	}
	return false, written, nil
}

// renewTransferLease refreshes the holder's lease part-way through a long
// side effect, so a revoke-all over many tokens cannot run past
// transferFulfillWindow and free the asset mid-flight. A lost claim is
// returned so the caller stops before its next side effect.
func (p *Plugin) renewTransferLease(lease transferLease) error {
	if !lease.active() {
		return nil
	}
	err := p.updateTransferClaim(lease.kind, lease.assetKey, lease.requestID, lease.attemptID, func(c *transferClaim) error {
		c.leaseAt = model.GetMillis()
		return nil
	})
	if err != nil {
		p.API.LogWarn("failed to renew transfer asset claim", "request_id", lease.requestID, "kind", lease.kind, "error", err.Error())
	}
	return err
}

// releaseTransferAssetClaim drops the claim when requestID still holds it.
// attemptID, when both it and the stored row carry one, has to match: an
// older paused attempt must not drop the row a newer one owns.
func (p *Plugin) releaseTransferAssetClaim(kind, assetKey, requestID, attemptID string) {
	if kind != pendingKindBotTransfer && kind != pendingKindWebhookTransfer {
		return
	}
	assetKey = strings.TrimSpace(assetKey)
	requestID = strings.TrimSpace(requestID)
	if assetKey == "" || requestID == "" {
		return
	}
	key := transferAssetClaimKey(kind, assetKey)
	raw, appErr := p.API.KVGet(key)
	if appErr != nil || raw == nil {
		return
	}
	if !parseTransferClaim(raw).ownedBy(requestID, attemptID) {
		return
	}
	// An incoming hook's ID is its secret URL and rides in assetKey, so the
	// claim key must never reach the log.
	if _, delErr := p.API.KVCompareAndDelete(key, raw); delErr != nil {
		p.API.LogWarn("failed to release transfer asset claim", "request_id", requestID, "kind", kind, "error", delErr.Error())
	}
}

// releaseTransferTicket drops the requester's pending list entry and the
// per-asset claim together. Ticket level: for callers that only close a
// pending ticket (expire, withdraw) and hold no fulfill attempt.
func (p *Plugin) releaseTransferTicket(kind, userID, requestID, assetKey string) {
	p.releasePending(kind, userID, requestID)
	p.releaseTransferAssetClaim(kind, assetKey, requestID, "")
}

// releaseTransferTicketLease closes the ticket for the attempt that holds
// the claim, so an older attempt coming back cannot free an asset a newer
// one is working on.
func (p *Plugin) releaseTransferTicketLease(lease transferLease, userID string) {
	p.releasePending(lease.kind, userID, lease.requestID)
	p.releaseTransferAssetClaim(lease.kind, lease.assetKey, lease.requestID, lease.attemptID)
}

// claimTransferForFulfill moves the claim into the fulfilling phase, then
// claims the pending record. The claim goes first so the asset is never
// unprotected while the record is gone. attemptID names the fulfill
// attempt that now owns the claim; the caller carries it through every
// renew and release that follows.
func (p *Plugin) claimTransferForFulfill(kind, assetKey, requestID, key string, rawReq []byte, cards decisionCards, what string) (res decisionResult, attemptID string, ok bool) {
	created, claimBytes, err := p.beginTransferFulfill(kind, assetKey, requestID)
	if err != nil {
		if errors.Is(err, errTransferClaimLost) {
			return decisionResult{Reply: "Another transfer ticket now owns this asset. Deny this request instead."}, "", false
		}
		if errors.Is(err, errTransferFulfillInFlight) {
			return decisionResult{Reply: "This request is already being carried out. Wait for that attempt to finish, then reload the card."}, "", false
		}
		p.API.LogError("failed to mark "+what+" as being carried out", "request_id", requestID, "error", err.Error())
		return decisionResult{Reply: "Could not process that request; try Approve again."}, "", false
	}
	res, ok = p.claimRecord(key, rawReq, cards, what)
	if !ok {
		p.undoFulfillClaim(kind, assetKey, requestID, key, created, claimBytes)
		return res, "", false
	}
	return res, parseTransferClaim(claimBytes).attemptID, true
}

// undoFulfillClaim takes the claim change back once it turns out the
// pending record was not this approval's to carry out.
func (p *Plugin) undoFulfillClaim(kind, assetKey, requestID, key string, created bool, claimBytes []byte) {
	if len(claimBytes) == 0 {
		return
	}
	if created {
		// This approval invented the claim row and then lost the record, so
		// the asset it is holding belongs to nobody. Take back exactly those
		// bytes; releasing by holder would also drop a row someone else
		// legitimately re-wrote in the meantime.
		p.deleteTransferClaimIfUnchanged(kind, assetKey, requestID, claimBytes)
		return
	}
	raw, appErr := p.API.KVGet(key)
	if appErr != nil || raw == nil {
		// The record is gone — the decision that won the claim owns the
		// asset now — or it cannot be read, which is no basis for rewriting
		// a claim row.
		return
	}
	// The ticket is still pending, so the move into fulfilling has to come
	// back out: a claim left fulfilling refuses Approve until its lease
	// ages out, for work that never started.
	held := parseTransferClaim(claimBytes)
	held.phase = transferPhaseHeld
	held.attemptID = ""
	held.leaseAt = model.GetMillis()
	// An incoming hook's ID is its secret URL and rides in assetKey, so the
	// claim key must never reach the log.
	if _, casErr := p.API.KVCompareAndSet(transferAssetClaimKey(kind, assetKey), claimBytes, held.encode()); casErr != nil {
		p.API.LogWarn("failed to put a transfer asset claim back after a lost record", "request_id", requestID, "kind", kind, "error", casErr.Error())
	}
}

// deleteTransferClaimIfUnchanged removes a claim row only while it still
// holds the exact bytes the caller wrote.
func (p *Plugin) deleteTransferClaimIfUnchanged(kind, assetKey, requestID string, raw []byte) {
	if len(raw) == 0 {
		return
	}
	// An incoming hook's ID is its secret URL and rides in assetKey, so the
	// claim key must never reach the log.
	if _, appErr := p.API.KVCompareAndDelete(transferAssetClaimKey(kind, assetKey), raw); appErr != nil {
		p.API.LogWarn("failed to drop a transfer asset claim after a lost record", "request_id", requestID, "kind", kind, "error", appErr.Error())
	}
}

// transferClaimStillHeld reports whether lease still owns the per-asset
// claim. A KV lookup error is returned separately so callers do not treat a
// transient failure as a stolen claim.
func (p *Plugin) transferClaimStillHeld(lease transferLease) (held bool, err error) {
	assetKey := strings.TrimSpace(lease.assetKey)
	requestID := strings.TrimSpace(lease.requestID)
	if assetKey == "" || requestID == "" {
		return false, nil
	}
	raw, appErr := p.API.KVGet(transferAssetClaimKey(lease.kind, assetKey))
	if appErr != nil {
		return false, errors.Wrap(appErr, "failed to read transfer asset claim")
	}
	if raw == nil {
		return false, nil
	}
	return parseTransferClaim(raw).ownedBy(requestID, lease.attemptID), nil
}

// restoreTransferRecord puts a claimed transfer ticket back after its
// approval could not be carried out. The record is published only while
// this request owns the claim and holds it in the restoring phase, which no
// takeover may break — so a lost claim never leaves two open tickets on one
// asset. The generation is bumped as the phase changes: a takeover that
// already read the claim and is about to CAS it away loses that race.
//
// restored is true only when the record is in place and this request owned
// the claim while it was written. err is set when the record could not be
// written, the claim could not be read, or the move into the restoring
// phase failed; nothing is published in those cases, because a record put
// back without confirmed ownership is how one asset ends up with two open
// tickets. restored is false with a nil err when another ticket holds the
// claim.
//
// A write that still fails after its retries leaves the claim alone. The
// side effects may already have moved the asset, so freeing it would let a
// second ticket act on a half-finished handover; the lease ages out
// instead.
func (p *Plugin) restoreTransferRecord(lease transferLease, key string, raw []byte) (restored bool, err error) {
	kind, assetKey, requestID := lease.kind, lease.assetKey, lease.requestID
	held, holdErr := p.transferClaimStillHeld(lease)
	if holdErr != nil {
		return false, holdErr
	}
	if !held {
		return false, nil
	}
	if phaseErr := p.updateTransferClaim(kind, assetKey, requestID, lease.attemptID, func(c *transferClaim) error {
		c.phase = transferPhaseRestoring
		c.generation++
		c.leaseAt = model.GetMillis()
		return nil
	}); phaseErr != nil {
		if errors.Is(phaseErr, errTransferClaimLost) {
			return false, nil
		}
		return false, phaseErr
	}
	if putErr := p.publishRestoredRecord(key, raw); putErr != nil {
		// The record could not be put back, so nothing names the handover
		// any more. The claim is deliberately left in place: a bot whose
		// ownership already moved must not have its asset handed to a
		// second ticket, and the lease ages out on its own if nobody picks
		// this one up.
		return false, putErr
	}
	if phaseErr := p.updateTransferClaim(kind, assetKey, requestID, lease.attemptID, func(c *transferClaim) error {
		c.phase = transferPhaseHeld
		// The record is back, so no attempt owns the asset any more.
		c.attemptID = ""
		c.leaseAt = model.GetMillis()
		return nil
	}); phaseErr != nil {
		if errors.Is(phaseErr, errTransferClaimLost) {
			// The claim went away under a restore that should have been
			// protected. Only another ticket owning the asset makes this
			// record a second open ticket: a newer attempt on this same
			// ticket legitimately republished it, and withdrawing the
			// bytes then would throw the ticket away.
			if p.transferClaimHeldByOtherRequest(kind, assetKey, requestID) {
				p.deleteTransferRecordIfUnchanged(key, raw)
			}
			return false, nil
		}
		// The record is published and this request still owns the claim;
		// the lease ages out on its own if nothing picks the ticket up.
		return true, phaseErr
	}
	return true, nil
}

// publishRestoredRecord writes a claimed ticket back, retrying a failed
// write while the claim is still held in the restoring phase. The record is
// what a later Approve loads to pick a part-finished handover up, so a
// single flaky write must not be what decides the ticket is gone for good.
func (p *Plugin) publishRestoredRecord(key string, raw []byte) error {
	var err error
	for range transferRecordPutRetries {
		if err = p.putPendingRequestKV(key, raw); err == nil {
			return nil
		}
	}
	return err
}

// transferClaimHeldByOtherRequest reports whether the asset claim now names
// a different ticket. A claim that cannot be read answers false: a record
// is only withdrawn on confirmed evidence that someone else owns the asset.
func (p *Plugin) transferClaimHeldByOtherRequest(kind, assetKey, requestID string) bool {
	raw, appErr := p.API.KVGet(transferAssetClaimKey(kind, assetKey))
	if appErr != nil || raw == nil {
		return false
	}
	holder := parseTransferClaim(raw).requestID
	return holder != "" && holder != requestID
}

// deleteTransferRecordIfUnchanged removes a record only when it still holds
// the exact bytes the caller wrote.
func (p *Plugin) deleteTransferRecordIfUnchanged(key string, raw []byte) {
	deleted, appErr := p.API.KVCompareAndDelete(key, raw)
	if appErr != nil {
		p.API.LogWarn("failed to withdraw a transfer record after a lost claim", "key", key, "error", appErr.Error())
		return
	}
	if deleted {
		p.untrackOpenRequestKey(key)
	}
}

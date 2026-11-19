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
// the holding request, a generation, the phase that request is in, and when
// its lease was last renewed:
//
//	requestID|generation|phase|leaseAt
//
// Submit stores the request record before claiming, so pendingRequestExists
// is true for the holder during the claim window. Final approval deletes
// the record before the REST side effects run, so the claim moves to the
// fulfilling phase first and the asset is never unprotected while the
// record is gone. Long side effects renew the lease as they go; a crash
// frees the asset once the lease ages out instead of locking it for good.
//
// Putting a record back after a failed fulfill runs in the restoring phase,
// which no takeover may break however stale the lease looks: the gap
// between "I still hold the claim" and the KV write is exactly where two
// tickets could end up on one asset.

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
)

// errTransferClaimLost reports that another ticket owns the asset claim.
var errTransferClaimLost = errors.New("another transfer ticket took over the asset claim")

// transferClaim is a decoded per-asset claim value.
type transferClaim struct {
	requestID string
	// generation rises each time a holder re-publishes its record, so a
	// takeover that already read the claim loses its CAS instead of
	// stealing a live ticket.
	generation int64
	phase      string
	leaseAt    int64 // millis of the last renewal
}

// transferLease names the claim a long-running fulfill has to keep
// renewing. The zero value turns renewal off (non-transfer callers).
type transferLease struct {
	kind      string
	assetKey  string
	requestID string
}

func (l transferLease) active() bool {
	return l.kind != "" && l.assetKey != "" && l.requestID != ""
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
		// and its REST calls, or putting the record back. Restoring must
		// never be interrupted, and anything else is protected until its
		// lease runs out.
		if claim.phase == transferPhaseRestoring || !claim.leaseExpired(model.GetMillis()) {
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

// updateTransferClaim CAS-applies fn to the claim while requestID still
// holds it, retrying when another write lands first. errTransferClaimLost
// means the asset moved on and the caller must stop before its next side
// effect.
func (p *Plugin) updateTransferClaim(kind, assetKey, requestID string, fn func(*transferClaim)) error {
	key := transferAssetClaimKey(kind, assetKey)
	for range transferClaimCASRetries {
		raw, appErr := p.API.KVGet(key)
		if appErr != nil {
			return errors.Wrap(appErr, "failed to read transfer asset claim")
		}
		claim := parseTransferClaim(raw)
		if claim.requestID != requestID {
			return errTransferClaimLost
		}
		next := claim
		fn(&next)
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
func (p *Plugin) beginTransferFulfill(kind, assetKey, requestID string) error {
	// A ticket filed before per-asset claims existed has no claim row, and
	// an unclaimed asset belongs to nobody: take it rather than refuse the
	// approval.
	fresh := transferClaim{requestID: requestID, phase: transferPhaseFulfilling, leaseAt: model.GetMillis()}
	ok, appErr := p.API.KVCompareAndSet(transferAssetClaimKey(kind, assetKey), nil, fresh.encode())
	if appErr != nil {
		return errors.Wrap(appErr, "failed to update transfer asset claim")
	}
	if ok {
		return nil
	}
	return p.updateTransferClaim(kind, assetKey, requestID, func(c *transferClaim) {
		c.phase = transferPhaseFulfilling
		c.leaseAt = model.GetMillis()
	})
}

// renewTransferLease refreshes the holder's lease part-way through a long
// side effect, so a revoke-all over many tokens cannot run past
// transferFulfillWindow and free the asset mid-flight. A lost claim is
// returned so the caller stops before its next side effect.
func (p *Plugin) renewTransferLease(lease transferLease) error {
	if !lease.active() {
		return nil
	}
	err := p.updateTransferClaim(lease.kind, lease.assetKey, lease.requestID, func(c *transferClaim) {
		c.leaseAt = model.GetMillis()
	})
	if err != nil {
		p.API.LogWarn("failed to renew transfer asset claim", "request_id", lease.requestID, "kind", lease.kind, "error", err.Error())
	}
	return err
}

// releaseTransferAssetClaim drops the claim when requestID still holds it.
func (p *Plugin) releaseTransferAssetClaim(kind, assetKey, requestID string) {
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
	if parseTransferClaim(raw).requestID != requestID {
		return
	}
	// An incoming hook's ID is its secret URL and rides in assetKey, so the
	// claim key must never reach the log.
	if _, delErr := p.API.KVCompareAndDelete(key, raw); delErr != nil {
		p.API.LogWarn("failed to release transfer asset claim", "request_id", requestID, "kind", kind, "error", delErr.Error())
	}
}

// releaseTransferTicket drops the requester's pending list entry and the
// per-asset claim together.
func (p *Plugin) releaseTransferTicket(kind, userID, requestID, assetKey string) {
	p.releasePending(kind, userID, requestID)
	p.releaseTransferAssetClaim(kind, assetKey, requestID)
}

// claimTransferForFulfill moves the claim into the fulfilling phase, then
// claims the pending record. The claim goes first so the asset is never
// unprotected while the record is gone.
func (p *Plugin) claimTransferForFulfill(kind, assetKey, requestID, key string, rawReq []byte, cards decisionCards, what string) (decisionResult, bool) {
	if err := p.beginTransferFulfill(kind, assetKey, requestID); err != nil {
		if errors.Is(err, errTransferClaimLost) {
			return decisionResult{Reply: "Another transfer ticket now owns this asset. Deny this request instead."}, false
		}
		p.API.LogError("failed to mark "+what+" as being carried out", "request_id", requestID, "error", err.Error())
		return decisionResult{Reply: "Could not process that request; try Approve again."}, false
	}
	return p.claimRecord(key, rawReq, cards, what)
}

// transferClaimStillHeld reports whether requestID still owns the per-asset
// claim. A KV lookup error is returned separately so callers do not treat a
// transient failure as a stolen claim.
func (p *Plugin) transferClaimStillHeld(kind, assetKey, requestID string) (held bool, err error) {
	assetKey = strings.TrimSpace(assetKey)
	requestID = strings.TrimSpace(requestID)
	if assetKey == "" || requestID == "" {
		return false, nil
	}
	raw, appErr := p.API.KVGet(transferAssetClaimKey(kind, assetKey))
	if appErr != nil {
		return false, errors.Wrap(appErr, "failed to read transfer asset claim")
	}
	if raw == nil {
		return false, nil
	}
	return parseTransferClaim(raw).requestID == requestID, nil
}

// restoreTransferRecord puts a claimed transfer ticket back after its
// approval could not be carried out. The record is published only while
// this request owns the claim and holds it in the restoring phase, which no
// takeover may break — so a lost claim never leaves two open tickets on one
// asset. The generation is bumped as the phase changes: a takeover that
// already read the claim and is about to CAS it away loses that race.
//
// restored is true when the record is in place (this request owns the claim,
// or ownership could not be confirmed and the record is kept for a retry).
// err is set when the record could not be written or the claim could not be
// read. restored is false with a nil err when another ticket holds the
// claim; nothing is written then.
func (p *Plugin) restoreTransferRecord(kind, assetKey, requestID, key string, raw []byte) (restored bool, err error) {
	held, holdErr := p.transferClaimStillHeld(kind, assetKey, requestID)
	if holdErr != nil {
		// Ownership unknown: keep the record so approvers can retry once KV
		// recovers, rather than dropping a ticket that may still be valid.
		if putErr := p.putPendingRequestKV(key, raw); putErr != nil {
			return false, putErr
		}
		return true, holdErr
	}
	if !held {
		return false, nil
	}
	if phaseErr := p.updateTransferClaim(kind, assetKey, requestID, func(c *transferClaim) {
		c.phase = transferPhaseRestoring
		c.generation++
		c.leaseAt = model.GetMillis()
	}); phaseErr != nil {
		if errors.Is(phaseErr, errTransferClaimLost) {
			return false, nil
		}
		if putErr := p.putPendingRequestKV(key, raw); putErr != nil {
			return false, putErr
		}
		return true, phaseErr
	}
	if putErr := p.putPendingRequestKV(key, raw); putErr != nil {
		// Free the asset so a stuck fulfill does not block it forever.
		p.releaseTransferAssetClaim(kind, assetKey, requestID)
		return false, putErr
	}
	if phaseErr := p.updateTransferClaim(kind, assetKey, requestID, func(c *transferClaim) {
		c.phase = transferPhaseHeld
		c.leaseAt = model.GetMillis()
	}); phaseErr != nil {
		if errors.Is(phaseErr, errTransferClaimLost) {
			// The claim went away under a restore that should have been
			// protected. Take back exactly the bytes just written so the
			// asset is not left with two open tickets.
			p.deleteTransferRecordIfUnchanged(key, raw)
			return false, nil
		}
		// The record is published and this request still owns the claim;
		// the lease ages out on its own if nothing picks the ticket up.
		return true, phaseErr
	}
	return true, nil
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

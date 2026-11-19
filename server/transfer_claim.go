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
// Submit stores the request record before claiming so pendingRequestExists
// is true for the holder during the claim window (avoids a second submitter
// treating an in-flight claim as stale).
//
// Final approval deletes the pending record before REST side effects run.
// During that window pendingRequestExists is false, so final approval first
// writes a fulfill marker for the request, and a stale overwrite is refused
// while that marker is fresh. The marker is timestamped rather than trusted
// forever, so a crash or restart mid-fulfill frees the asset once
// transferFulfillWindow has passed instead of locking it for good.

const (
	kvTransferAssetClaimPrefix = "transfer_asset_claim_"
	kvTransferFulfillingPrefix = "transfer_fulfilling_"

	// transferFulfillWindow bounds how long a fulfill marker protects a
	// claim. Revoke-all walks every token on a bot, each call capped by
	// transferRESTTimeout, so the window is generous and the marker is
	// renewed as the loop runs (renewTransferFulfilling).
	transferFulfillWindow = 60 * time.Minute

	transferClaimCASRetries = 5
)

// errTransferClaimLost reports that another ticket owns the asset claim.
var errTransferClaimLost = errors.New("another transfer ticket took over the asset claim")

// Claim values are "requestID|generation". The generation rises each time a
// holder re-publishes its record after a failed fulfill, so a takeover that
// already read the claim loses its CAS instead of stealing a live ticket.
// Values written before versioning are bare request IDs (generation 0).
func formatTransferClaim(requestID string, gen int64) string {
	return requestID + "|" + strconv.FormatInt(gen, 10)
}

func parseTransferClaim(raw []byte) (requestID string, gen int64) {
	value := strings.TrimSpace(string(raw))
	id, genPart, versioned := strings.Cut(value, "|")
	id = strings.TrimSpace(id)
	if !versioned {
		return id, 0
	}
	parsed, parseErr := strconv.ParseInt(strings.TrimSpace(genPart), 10, 64)
	if parseErr != nil {
		return id, 0
	}
	return id, parsed
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

func transferFulfillingKey(requestID string) string {
	return kvTransferFulfillingPrefix + requestID
}

// markTransferFulfilling records that requestID's final approval is about
// to delete its pending record and run the side effects. It must be written
// before the record is claimed.
func (p *Plugin) markTransferFulfilling(requestID string) error {
	stamp := strconv.FormatInt(model.GetMillis(), 10)
	if appErr := p.API.KVSet(transferFulfillingKey(requestID), []byte(stamp)); appErr != nil {
		return errors.Wrap(appErr, "failed to mark transfer as being carried out")
	}
	return nil
}

// renewTransferFulfilling refreshes requestID's fulfill marker part-way
// through a long side effect, so a revoke-all over many tokens cannot run
// past transferFulfillWindow and free the asset mid-flight.
func (p *Plugin) renewTransferFulfilling(requestID string) {
	if strings.TrimSpace(requestID) == "" {
		return
	}
	if err := p.markTransferFulfilling(requestID); err != nil {
		p.API.LogWarn("failed to renew transfer fulfill marker", "request_id", requestID, "error", err.Error())
	}
}

// clearTransferFulfilling drops requestID's fulfill marker.
func (p *Plugin) clearTransferFulfilling(requestID string) {
	if appErr := p.API.KVDelete(transferFulfillingKey(requestID)); appErr != nil {
		p.API.LogWarn("failed to clear transfer fulfill marker", "request_id", requestID, "error", appErr.Error())
	}
}

// transferFulfillInProgress reports whether requestID's final approval
// started within transferFulfillWindow.
func (p *Plugin) transferFulfillInProgress(requestID string) (bool, error) {
	raw, appErr := p.API.KVGet(transferFulfillingKey(requestID))
	if appErr != nil {
		return false, errors.Wrap(appErr, "failed to read transfer fulfill marker")
	}
	if raw == nil {
		return false, nil
	}
	started, parseErr := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if parseErr != nil {
		// Unreadable marker: treat as in progress until it is cleared.
		return true, nil
	}
	return model.GetMillis()-started < transferFulfillWindow.Milliseconds(), nil
}

// claimTransferAsset CAS-writes requestID as the holder of assetKey. If
// another live request already holds the claim (or another open ticket
// covers the asset), returns a duplicate error. Stale claims are overwritten
// only when the holder request is gone, it is not being carried out, and no
// other open ticket covers the asset.
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

	ok, appErr := p.API.KVCompareAndSet(key, nil, []byte(formatTransferClaim(requestID, 0)))
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
	holder, _ := parseTransferClaim(raw)
	if holder == requestID {
		return nil
	}
	if holder == "" {
		ok, appErr = p.API.KVCompareAndSet(key, raw, []byte(formatTransferClaim(requestID, 0)))
		if appErr != nil {
			return errors.New("could not claim that asset; try again")
		}
		if !ok {
			return duplicateRequestError(kind, assetKey)
		}
		return nil
	}

	exists, err := p.pendingRequestExists(kind, holder)
	if err != nil {
		return errors.New("could not check for an existing transfer ticket; try again")
	}
	if exists {
		return duplicateRequestError(kind, assetKey)
	}
	// Holder record missing: refuse while its final approval is still
	// being carried out (the record is deleted before the REST calls).
	fulfilling, fulfillErr := p.transferFulfillInProgress(holder)
	if fulfillErr != nil {
		return errors.New("could not check for an existing transfer ticket; try again")
	}
	if fulfilling {
		return duplicateRequestError(kind, assetKey)
	}
	held, holdErr := p.transferAssetPendingElsewhere(kind, assetKey, requestID)
	if holdErr != nil {
		return errors.New("could not check for an existing transfer ticket; try again")
	}
	if held {
		return duplicateRequestError(kind, assetKey)
	}

	ok, appErr = p.API.KVCompareAndSet(key, raw, []byte(formatTransferClaim(requestID, 0)))
	if appErr != nil {
		return errors.New("could not claim that asset; try again")
	}
	if !ok {
		return duplicateRequestError(kind, assetKey)
	}
	return nil
}

// bumpTransferClaimGen raises the claim's generation while requestID still
// owns it, so a takeover that already read the old value cannot CAS it away
// while the holder puts its record back.
func (p *Plugin) bumpTransferClaimGen(kind, assetKey, requestID string) error {
	key := transferAssetClaimKey(kind, assetKey)
	for range transferClaimCASRetries {
		raw, appErr := p.API.KVGet(key)
		if appErr != nil {
			return errors.Wrap(appErr, "failed to read transfer asset claim")
		}
		holder, gen := parseTransferClaim(raw)
		if holder != requestID {
			return errTransferClaimLost
		}
		ok, casErr := p.API.KVCompareAndSet(key, raw, []byte(formatTransferClaim(requestID, gen+1)))
		if casErr != nil {
			return errors.Wrap(casErr, "failed to update transfer asset claim")
		}
		if ok {
			return nil
		}
	}
	return errors.New("failed to update transfer asset claim after concurrent updates")
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
	if holder, _ := parseTransferClaim(raw); holder != requestID {
		return
	}
	// An incoming hook's ID is its secret URL and rides in assetKey, so the
	// claim key must never reach the log.
	if _, delErr := p.API.KVCompareAndDelete(key, raw); delErr != nil {
		p.API.LogWarn("failed to release transfer asset claim", "request_id", requestID, "kind", kind, "error", delErr.Error())
	}
}

// releaseTransferTicket drops the requester's pending list entry, the
// per-asset claim and any fulfill marker together.
func (p *Plugin) releaseTransferTicket(kind, userID, requestID, assetKey string) {
	p.releasePending(kind, userID, requestID)
	p.releaseTransferAssetClaim(kind, assetKey, requestID)
	p.clearTransferFulfilling(requestID)
}

// claimTransferForFulfill marks requestID as being carried out, then claims
// its pending record. The marker goes first so the asset claim is never
// unprotected while the record is gone. On a lost claim the marker is left
// for the winner (or the window) to clear: a concurrent final approval of
// the same request shares it.
func (p *Plugin) claimTransferForFulfill(requestID, key string, rawReq []byte, cards decisionCards, what string) (decisionResult, bool) {
	if err := p.markTransferFulfilling(requestID); err != nil {
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
	holder, _ := parseTransferClaim(raw)
	return holder == requestID, nil
}

// restoreTransferRecord puts a claimed transfer ticket back after its
// approval could not be carried out. The record is published only once this
// request is confirmed to still own the asset claim, so a lost claim never
// leaves two open tickets on one asset. The claim generation is bumped
// first: a takeover that already read the claim and is about to CAS it away
// loses that race instead of stealing the ticket back.
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
	if bumpErr := p.bumpTransferClaimGen(kind, assetKey, requestID); bumpErr != nil {
		if errors.Is(bumpErr, errTransferClaimLost) {
			return false, nil
		}
		if putErr := p.putPendingRequestKV(key, raw); putErr != nil {
			return false, putErr
		}
		return true, bumpErr
	}
	if putErr := p.putPendingRequestKV(key, raw); putErr != nil {
		// Free the asset so a stuck fulfill does not block it forever.
		p.releaseTransferAssetClaim(kind, assetKey, requestID)
		return false, putErr
	}
	return true, nil
}

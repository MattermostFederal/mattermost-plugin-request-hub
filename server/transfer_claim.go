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
	// claim. It is well above the longest fulfill (a handful of REST calls,
	// each capped by transferRESTTimeout).
	transferFulfillWindow = 15 * time.Minute
)

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

	ok, appErr := p.API.KVCompareAndSet(key, nil, []byte(requestID))
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
	holder := strings.TrimSpace(string(raw))
	if holder == requestID {
		return nil
	}
	if holder == "" {
		ok, appErr = p.API.KVCompareAndSet(key, raw, []byte(requestID))
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

	ok, appErr = p.API.KVCompareAndSet(key, raw, []byte(requestID))
	if appErr != nil {
		return errors.New("could not claim that asset; try again")
	}
	if !ok {
		return duplicateRequestError(kind, assetKey)
	}
	return nil
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
	if strings.TrimSpace(string(raw)) != requestID {
		return
	}
	if _, delErr := p.API.KVCompareAndDelete(key, raw); delErr != nil {
		p.API.LogWarn("failed to release transfer asset claim", "key", key, "error", delErr.Error())
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
	return strings.TrimSpace(string(raw)) == requestID, nil
}

// restoreTransferRecord puts a claimed transfer ticket back after its
// approval could not be carried out. The record goes back before the asset
// claim is re-checked: a submit that saw the record missing and took the
// claim in between is caught by the re-check, and one that comes after
// sees the record and is refused.
//
// restored is true when this request still owns the claim (or ownership
// could not be confirmed). err is set when the record could not be written,
// or when the claim check failed after a successful write (record left in
// place for retry). restored is false with a nil err when another ticket
// holds the claim and this record was removed again.
func (p *Plugin) restoreTransferRecord(kind, assetKey, requestID, key string, raw []byte) (restored bool, err error) {
	if err := p.putPendingRequestKV(key, raw); err != nil {
		// Best-effort: free the asset if we still own the claim so a stuck
		// fulfill does not block the asset forever.
		if held, holdErr := p.transferClaimStillHeld(kind, assetKey, requestID); holdErr == nil && held {
			p.releaseTransferAssetClaim(kind, assetKey, requestID)
		}
		return false, err
	}
	held, holdErr := p.transferClaimStillHeld(kind, assetKey, requestID)
	if holdErr != nil {
		// Record is back; keep it so approvers can retry once KV recovers.
		return true, holdErr
	}
	if held {
		return true, nil
	}
	deleted, appErr := p.API.KVCompareAndDelete(key, raw)
	switch {
	case appErr != nil:
		p.API.LogError("failed to remove transfer request that lost its asset claim", "request_id", requestID, "error", appErr.Error())
		if delErr := p.API.KVDelete(key); delErr != nil {
			p.API.LogError("failed to force-delete transfer request that lost its asset claim", "request_id", requestID, "error", delErr.Error())
		} else {
			p.untrackOpenRequestKey(key)
		}
	case deleted:
		p.untrackOpenRequestKey(key)
	}
	return false, nil
}

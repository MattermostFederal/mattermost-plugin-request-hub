package main

import (
	"strings"

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
// During that window pendingRequestExists is false, so stale overwrite is
// gated on the holder's request history being terminal — otherwise a
// concurrent submit would steal the claim mid-fulfill.

const kvTransferAssetClaimPrefix = "transfer_asset_claim_"

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

// transferHistoryTerminal reports whether a request's history shows it has
// left the open/in-flight set, so its asset claim may be treated as stale.
func transferHistoryTerminal(status string) bool {
	switch status {
	case historyApproved, historyDenied, historyWithdrawn, historyAcknowledged,
		historyExpired, historyFailed, historyLost, historyClosed:
		return true
	default:
		return false
	}
}

// claimTransferAsset CAS-writes requestID as the holder of assetKey. If
// another live request already holds the claim (or another open ticket
// covers the asset), returns a duplicate error. Stale claims are overwritten
// only when the holder request is gone, its history is terminal, and no
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
	// Holder record missing: refuse while history is still open/in-flight
	// (final approval deletes the record before REST calls finish).
	h, histErr := p.loadHistory(holder)
	if histErr != nil {
		return errors.New("could not check for an existing transfer ticket; try again")
	}
	if h == nil || !transferHistoryTerminal(h.Status) {
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

// releaseTransferTicket drops the requester's pending list entry and the
// per-asset claim together.
func (p *Plugin) releaseTransferTicket(kind, userID, requestID, assetKey string) {
	p.releasePending(kind, userID, requestID)
	p.releaseTransferAssetClaim(kind, assetKey, requestID)
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

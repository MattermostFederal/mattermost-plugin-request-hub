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

// claimTransferAsset CAS-writes requestID as the holder of assetKey. If
// another live request already holds the claim (or another open ticket
// covers the asset), returns a duplicate error. Stale claims are overwritten
// only when the holder request is gone and no other open ticket covers the asset.
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
	// Holder record missing: still refuse if another open ticket covers the
	// asset (claim→store race on an older build, or a concurrent store).
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
// claim. Used by restore-after-failure so a concurrent submit that stole the
// claim during fulfill cannot leave two live tickets.
func (p *Plugin) transferClaimStillHeld(kind, assetKey, requestID string) bool {
	assetKey = strings.TrimSpace(assetKey)
	requestID = strings.TrimSpace(requestID)
	if assetKey == "" || requestID == "" {
		return false
	}
	raw, appErr := p.API.KVGet(transferAssetClaimKey(kind, assetKey))
	if appErr != nil || raw == nil {
		return false
	}
	return strings.TrimSpace(string(raw)) == requestID
}

// restoreTransferRecord puts a claimed transfer ticket back after its
// approval could not be carried out. The record goes back before the asset
// claim is re-checked: a submit that saw the record missing and took the
// claim in between is caught by the re-check, and one that comes after
// sees the record and is refused, so one asset never has two open tickets.
// restored is false when another ticket holds the claim; the record is
// then removed again.
func (p *Plugin) restoreTransferRecord(kind, assetKey, requestID, key string, raw []byte) (restored bool, err error) {
	if err := p.putPendingRequestKV(key, raw); err != nil {
		return false, err
	}
	if p.transferClaimStillHeld(kind, assetKey, requestID) {
		return true, nil
	}
	deleted, appErr := p.API.KVCompareAndDelete(key, raw)
	switch {
	case appErr != nil:
		p.API.LogError("failed to remove transfer request that lost its asset claim", "request_id", requestID, "error", appErr.Error())
	case deleted:
		p.untrackOpenRequestKey(key)
	}
	return false, nil
}

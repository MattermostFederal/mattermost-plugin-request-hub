package main

import (
	"strings"

	"github.com/pkg/errors"
)

// Atomic per-asset lock for bot/webhook transfer tickets. Soft duplicate
// scans (transferAssetPendingElsewhere) race under concurrent submit; this
// CAS claim is the source of truth for "one open ticket per asset".

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
// another live request already holds the claim, returns a duplicate error.
// Stale claims (holder request gone) are overwritten.
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
	if holder == "" || holder == requestID {
		// Empty or re-claim of our own ID: force-set.
		if setErr := p.API.KVSet(key, []byte(requestID)); setErr != nil {
			return errors.New("could not claim that asset; try again")
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

	// Stale lock: replace only if still the same holder bytes.
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

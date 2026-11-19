package main

import (
	"encoding/json"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

// Standalone webhooks (those created by a webhook request, not bundled with a
// bot) had no Request Hub index. Without one, transfer/retire tickets can't
// find them, so each successful standalone create now writes a
// webhookIndexEntry keyed by hook ID.

const (
	kvWebhookIndexPrefix = "webhook_index_"
	webhookIndexPageSize = 200
	// kvWebhookTransferHandlePrefix maps an opaque public handle → hook ID so
	// transferable_assets never returns an incoming webhook's secret URL id.
	kvWebhookTransferHandlePrefix = "webhook_xfer_handle_"
)

// webhookIndexEntry links a standalone webhook RH created to its request and
// keeps who owns it, so it can be listed, transferred, retired, and flagged
// orphaned by hook ID after the request history is gone.
type webhookIndexEntry struct {
	HookID      string `json:"hook_id"`
	WebhookType string `json:"webhook_type"` // incoming / outgoing
	ChannelID   string `json:"channel_id,omitempty"`
	TeamID      string `json:"team_id,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
	// CallbackURL is the outgoing webhook callback (first URL). Stored so
	// retire+recreate can rebuild the hook without a live GetOutgoingWebhook.
	CallbackURL string `json:"callback_url,omitempty"`
	OwnerUserID string `json:"owner_user_id"`
	RequesterID string `json:"requester_id,omitempty"`
	RequestID   string `json:"request_id,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	// TransferHandle is an opaque public id for incoming hooks (the real
	// HookID is the secret URL). Empty for outgoing hooks, which keep HookID
	// as the picker asset id.
	TransferHandle string `json:"transfer_handle,omitempty"`
	// RetiredAt marks a retired hook without deleting the index, so transfer
	// pickers skip it. Zero means live.
	RetiredAt int64 `json:"retired_at,omitempty"`
}

func webhookIndexKey(hookID string) string { return kvWebhookIndexPrefix + hookID }

// writeWebhookIndex records a standalone webhook created for req. Exercise
// hooks are tracked in the exercise manifest instead. Returns an error when
// the index could not be written (caller may surface as a partial approval).
func (p *Plugin) writeWebhookIndex(req *webhookRequest, hookID, approverID string) error {
	if req.Exercise || strings.TrimSpace(hookID) == "" {
		return nil
	}
	entry := webhookIndexEntry{
		HookID:      hookID,
		WebhookType: req.WebhookType,
		ChannelID:   req.ChannelID,
		TeamID:      req.TeamID,
		DisplayName: req.DisplayName,
		Description: strings.TrimSpace(req.Description),
		CallbackURL: strings.TrimSpace(req.CallbackURL),
		OwnerUserID: req.RequesterID,
		RequesterID: req.RequesterID,
		RequestID:   req.ID,
		CreatedAt:   model.GetMillis(),
	}
	if req.WebhookType == webhookTypeIncoming {
		entry.TransferHandle = model.NewId()
	}
	if err := p.storeWebhookIndex(entry); err != nil {
		p.API.LogError("failed to write webhook index entry", "hook_id", hookID, "request_id", req.ID, "approver_id", approverID, "error", err.Error())
		return err
	}
	return nil
}

func (p *Plugin) storeWebhookIndex(entry webhookIndexEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return errors.Wrap(err, "failed to marshal webhook index entry")
	}
	if appErr := p.API.KVSet(webhookIndexKey(entry.HookID), data); appErr != nil {
		return errors.Wrap(appErr, "failed to store webhook index entry")
	}
	p.trackWebhookIndexKey(entry.HookID)
	if entry.WebhookType == webhookTypeIncoming && entry.TransferHandle != "" {
		// Best-effort: list path can recreate the handle map via
		// ensureIncomingTransferHandle if this write is skipped.
		if appErr := p.API.KVSet(kvWebhookTransferHandlePrefix+entry.TransferHandle, []byte(entry.HookID)); appErr != nil {
			p.API.LogWarn("failed to store webhook transfer handle", "hook_id", entry.HookID, "error", appErr.Error())
		}
	}
	return nil
}

// storeWebhookIndexIfAbsent writes the index only when no row exists yet
// (CAS from nil). Used by backfill so a concurrent reassign/retire wins.
func (p *Plugin) storeWebhookIndexIfAbsent(entry webhookIndexEntry) (wrote bool, err error) {
	data, err := json.Marshal(entry)
	if err != nil {
		return false, errors.Wrap(err, "failed to marshal webhook index entry")
	}
	ok, appErr := p.API.KVCompareAndSet(webhookIndexKey(entry.HookID), nil, data)
	if appErr != nil {
		return false, errors.Wrap(appErr, "failed to store webhook index entry")
	}
	if !ok {
		return false, nil
	}
	p.trackWebhookIndexKey(entry.HookID)
	if entry.WebhookType == webhookTypeIncoming && entry.TransferHandle != "" {
		_, _ = p.API.KVCompareAndSet(kvWebhookTransferHandlePrefix+entry.TransferHandle, nil, []byte(entry.HookID))
	}
	return true, nil
}

// loadWebhookIndex returns the index entry for a hook, or nil when RH has none.
func (p *Plugin) loadWebhookIndex(hookID string) (*webhookIndexEntry, error) {
	data, appErr := p.API.KVGet(webhookIndexKey(hookID))
	if appErr != nil {
		return nil, appErr
	}
	if data == nil {
		return nil, nil
	}
	var entry webhookIndexEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal webhook index entry")
	}
	return &entry, nil
}

// listWebhookIndex returns every webhook index entry. Unreadable entries are
// logged and skipped. Prefers the compact webhook-index key list so pickers
// do not page the entire plugin KV store.
func (p *Plugin) listWebhookIndex() ([]webhookIndexEntry, error) {
	keys, err := p.listWebhookIndexKeys()
	if err != nil {
		return nil, err
	}
	var out []webhookIndexEntry
	for _, key := range keys {
		if !strings.HasPrefix(key, kvWebhookIndexPrefix) {
			continue
		}
		entry, loadErr := p.loadWebhookIndex(strings.TrimPrefix(key, kvWebhookIndexPrefix))
		if loadErr != nil {
			p.API.LogWarn("skipping unreadable webhook index entry", "key", key, "error", loadErr.Error())
			continue
		}
		if entry != nil {
			out = append(out, *entry)
		}
	}
	return out, nil
}

// retireWebhookIndex marks a hook retired. Returns an error when the index
// could not be updated (caller may surface as a partial approval).
func (p *Plugin) retireWebhookIndex(hookID string) error {
	entry, err := p.loadWebhookIndex(hookID)
	if err != nil {
		return err
	}
	if entry == nil {
		return nil
	}
	entry.RetiredAt = model.GetMillis()
	if err := p.storeWebhookIndex(*entry); err != nil {
		p.API.LogWarn("failed to mark webhook index retired", "hook_id", hookID, "error", err.Error())
		return err
	}
	return nil
}

// updateWebhookIndexOwner records a new owner on a webhook index entry.
func (p *Plugin) updateWebhookIndexOwner(hookID, newOwnerID string) error {
	entry, err := p.loadWebhookIndex(hookID)
	if err != nil {
		return err
	}
	if entry == nil {
		return errors.New("webhook index entry not found")
	}
	entry.OwnerUserID = newOwnerID
	if storeErr := p.storeWebhookIndex(*entry); storeErr != nil {
		p.API.LogWarn("failed to update webhook index owner", "hook_id", hookID, "error", storeErr.Error())
		return storeErr
	}
	return nil
}

// ensureIncomingTransferHandle returns the opaque public asset id for an
// incoming hook, creating and persisting a handle when older index rows lack
// one. Outgoing hooks keep returning the real HookID.
func (p *Plugin) ensureIncomingTransferHandle(entry *webhookIndexEntry) string {
	if entry == nil {
		return ""
	}
	if entry.WebhookType != webhookTypeIncoming {
		return entry.HookID
	}
	if entry.TransferHandle != "" {
		return entry.TransferHandle
	}
	entry.TransferHandle = model.NewId()
	if err := p.storeWebhookIndex(*entry); err != nil {
		p.API.LogWarn("failed to persist incoming webhook transfer handle", "hook_id", entry.HookID, "error", err.Error())
	}
	return entry.TransferHandle
}

// resolveWebhookTransferHookID maps a picker asset id (opaque handle or raw
// hook id) to the real Mattermost hook id.
func (p *Plugin) resolveWebhookTransferHookID(assetID string) (string, error) {
	assetID = strings.TrimSpace(assetID)
	if assetID == "" {
		return "", errors.New("pick a webhook")
	}
	if entry, err := p.loadWebhookIndex(assetID); err != nil {
		return "", errors.New("could not look up that webhook; try again")
	} else if entry != nil {
		return entry.HookID, nil
	}
	raw, appErr := p.API.KVGet(kvWebhookTransferHandlePrefix + assetID)
	if appErr != nil {
		return "", errors.New("could not look up that webhook; try again")
	}
	if raw != nil {
		hookID := strings.TrimSpace(string(raw))
		if hookID != "" {
			return hookID, nil
		}
	}
	return "", errors.New("that webhook isn't managed by Request Hub, so it can't be transferred or retired here")
}

package main

import (
	"encoding/json"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

// Standalone webhooks (those created by a webhook request, not bundled with a
// bot) had no Request Hub index. Without one, transfer/retire tickets and the
// orphaned-owner scanner can't find them, so each successful standalone create
// now writes a webhookIndexEntry keyed by hook ID.

const (
	kvWebhookIndexPrefix = "webhook_index_"
	webhookIndexPageSize = 200
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
	// CallbackURL is the outgoing webhook callback (first URL). Stored so
	// retire+recreate can rebuild the hook without a live GetOutgoingWebhook.
	CallbackURL string `json:"callback_url,omitempty"`
	OwnerUserID string `json:"owner_user_id"`
	RequesterID string `json:"requester_id,omitempty"`
	RequestID   string `json:"request_id,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	// RetiredAt marks a retired hook without deleting the index, so audits
	// and the orphan scanner skip it. Zero means live.
	RetiredAt int64 `json:"retired_at,omitempty"`
}

func webhookIndexKey(hookID string) string { return kvWebhookIndexPrefix + hookID }

// writeWebhookIndex records a standalone webhook created for req. Exercise
// hooks are tracked in the exercise manifest instead. A failure is logged:
// the hook exists either way, and its request history still links it.
func (p *Plugin) writeWebhookIndex(req *webhookRequest, hookID, approverID string) {
	if req.Exercise || strings.TrimSpace(hookID) == "" {
		return
	}
	entry := webhookIndexEntry{
		HookID:      hookID,
		WebhookType: req.WebhookType,
		ChannelID:   req.ChannelID,
		TeamID:      req.TeamID,
		DisplayName: req.DisplayName,
		CallbackURL: strings.TrimSpace(req.CallbackURL),
		OwnerUserID: req.RequesterID,
		RequesterID: req.RequesterID,
		RequestID:   req.ID,
		CreatedAt:   model.GetMillis(),
	}
	if err := p.storeWebhookIndex(entry); err != nil {
		p.API.LogError("failed to write webhook index entry", "hook_id", hookID, "request_id", req.ID, "approver_id", approverID, "error", err.Error())
	}
}

func (p *Plugin) storeWebhookIndex(entry webhookIndexEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return errors.Wrap(err, "failed to marshal webhook index entry")
	}
	if appErr := p.API.KVSet(webhookIndexKey(entry.HookID), data); appErr != nil {
		return errors.Wrap(appErr, "failed to store webhook index entry")
	}
	return nil
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
// logged and skipped.
func (p *Plugin) listWebhookIndex() ([]webhookIndexEntry, error) {
	var out []webhookIndexEntry
	for page := 0; ; page++ {
		keys, appErr := p.API.KVList(page, webhookIndexPageSize)
		if appErr != nil {
			return nil, appErr
		}
		for _, key := range keys {
			if !strings.HasPrefix(key, kvWebhookIndexPrefix) {
				continue
			}
			entry, err := p.loadWebhookIndex(strings.TrimPrefix(key, kvWebhookIndexPrefix))
			if err != nil {
				p.API.LogWarn("skipping unreadable webhook index entry", "key", key, "error", err.Error())
				continue
			}
			if entry != nil {
				out = append(out, *entry)
			}
		}
		if len(keys) < webhookIndexPageSize {
			return out, nil
		}
	}
}

// retireWebhookIndex marks a hook retired (best-effort).
func (p *Plugin) retireWebhookIndex(hookID string) {
	entry, err := p.loadWebhookIndex(hookID)
	if err != nil || entry == nil {
		return
	}
	entry.RetiredAt = model.GetMillis()
	if err := p.storeWebhookIndex(*entry); err != nil {
		p.API.LogWarn("failed to mark webhook index retired", "hook_id", hookID, "error", err.Error())
	}
}

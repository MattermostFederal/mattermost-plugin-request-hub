package main

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
)

const kvWebhookIndexBackfillDone = "migration_webhook_index_backfill_v1"

// backfillWebhookIndexFromHistory writes webhook_index_* rows for standalone
// webhooks that were approved before the index existed. One-shot per
// deployment (marker key); skips exercise hooks, incomplete snapshots, and
// already-indexed hooks. Uses CAS-from-nil so a concurrent reassign/retire wins.
func (p *Plugin) backfillWebhookIndexFromHistory() {
	raw, appErr := p.API.KVGet(kvWebhookIndexBackfillDone)
	if appErr != nil {
		p.API.LogWarn("webhook index backfill: marker read failed", "error", appErr.Error())
		return
	}
	if raw != nil {
		return
	}

	// A transient error leaves the marker unset so the next activation
	// retries; every write is create-if-absent, so a rerun is safe.
	hadErr := false
	scan := func() (completed bool) {
		for page := 0; ; page++ {
			keys, listErr := p.API.KVList(page, webhookIndexPageSize)
			if listErr != nil {
				p.API.LogWarn("webhook index backfill: KVList failed", "error", listErr.Error())
				return false
			}
			p.backfillWebhookIndexKeys(keys, &hadErr)
			if len(keys) < webhookIndexPageSize {
				return true
			}
		}
	}

	// KV offset paging has no stable order, so rows that shift between pages
	// can be skipped. Two full passes (create-if-absent both times) catch
	// what one missed; the marker is only written when both finish clean.
	for range 2 {
		if !scan() {
			return
		}
	}
	if hadErr {
		return
	}
	if setErr := p.API.KVSet(kvWebhookIndexBackfillDone, []byte("1")); setErr != nil {
		p.API.LogWarn("webhook index backfill: failed to set done marker", "error", setErr.Error())
	}
}

// backfillWebhookIndexKeys indexes the approved webhook requests among keys.
// hadErr is raised when a row could not be read or written, so the caller
// leaves the done marker unset and retries on the next activation.
func (p *Plugin) backfillWebhookIndexKeys(keys []string, hadErr *bool) {
	for _, key := range keys {
		if !strings.HasPrefix(key, kvHistoryPrefix) {
			continue
		}
		h, err := p.loadHistory(strings.TrimPrefix(key, kvHistoryPrefix))
		if err != nil {
			p.API.LogWarn("webhook index backfill: history load failed", "key", key, "error", err.Error())
			*hadErr = true
			continue
		}
		if h == nil || h.Kind != pendingKindWebhook || h.Status != historyApproved {
			continue
		}
		hookID := webhookHookIDFromHistory(h)
		if hookID == "" {
			continue
		}
		existing, loadErr := p.loadWebhookIndex(hookID)
		if loadErr != nil {
			p.API.LogWarn("webhook index backfill: index load failed", "request_id", h.RequestID, "error", loadErr.Error())
			*hadErr = true
			continue
		}
		if existing != nil {
			continue
		}
		var snap webhookRequest
		if len(h.Request) == 0 {
			continue
		}
		if uErr := json.Unmarshal(h.Request, &snap); uErr != nil {
			p.API.LogWarn("webhook index backfill: skipping unreadable request snapshot", "request_id", h.RequestID, "error", uErr.Error())
			continue
		}
		if snap.Exercise {
			continue
		}
		if strings.TrimSpace(snap.WebhookType) == "" || (snap.WebhookType != webhookTypeIncoming && snap.WebhookType != webhookTypeOutgoing) {
			continue
		}
		if snap.RequesterID == "" {
			snap.RequesterID = h.RequesterID
		}
		if snap.ID == "" {
			snap.ID = h.RequestID
		}
		entry := webhookIndexEntry{
			HookID:      hookID,
			WebhookType: snap.WebhookType,
			ChannelID:   snap.ChannelID,
			TeamID:      snap.TeamID,
			DisplayName: snap.DisplayName,
			Description: strings.TrimSpace(snap.Description),
			CallbackURL: strings.TrimSpace(snap.CallbackURL),
			OwnerUserID: snap.RequesterID,
			RequesterID: snap.RequesterID,
			RequestID:   snap.ID,
			CreatedAt:   model.GetMillis(),
		}
		if snap.WebhookType == webhookTypeIncoming {
			entry.TransferHandle = model.NewId()
		}
		if _, storeErr := p.storeWebhookIndexIfAbsent(entry); storeErr != nil {
			p.API.LogWarn("webhook index backfill: store failed", "request_id", h.RequestID, "error", storeErr.Error())
			*hadErr = true
		}
	}
}

func webhookHookIDFromHistory(h *requestHistory) string {
	for _, ev := range slices.Backward(h.Events) {
		if ev.Event != eventApproved && ev.Event != eventApprovedPartial {
			continue
		}
		if ev.Detail == nil {
			continue
		}
		if id, ok := ev.Detail["hook_id"].(string); ok {
			if id = strings.TrimSpace(id); id != "" && model.IsValidId(id) {
				return id
			}
		}
	}
	return ""
}

package main

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
)

// backfillWebhookIndexFromHistory writes webhook_index_* rows for standalone
// webhooks that were approved before the index existed. It skips exercise
// hooks, incomplete snapshots, and already-indexed hooks, and writes with
// CAS-from-nil so a concurrent reassign/retire wins.
//
// There is no done marker. KV offset paging has no stable order, so rows
// that shift between pages are skipped, and no single run can prove it saw
// everything — a marker would make a miss permanent. Every write is
// create-if-absent, so reconciling on each activation is cheap and lets a
// later run pick up what an earlier one missed.
func (p *Plugin) backfillWebhookIndexFromHistory() {
	for page := 0; ; page++ {
		keys, listErr := p.API.KVList(page, webhookIndexPageSize)
		if listErr != nil {
			p.API.LogWarn("webhook index backfill: KVList failed", "error", listErr.Error())
			return
		}
		p.backfillWebhookIndexKeys(keys)
		if len(keys) < webhookIndexPageSize {
			return
		}
	}
}

// backfillWebhookIndexKeys indexes the approved webhook requests among keys.
func (p *Plugin) backfillWebhookIndexKeys(keys []string) {
	for _, key := range keys {
		if !strings.HasPrefix(key, kvHistoryPrefix) {
			continue
		}
		h, err := p.loadHistory(strings.TrimPrefix(key, kvHistoryPrefix))
		if err != nil {
			p.API.LogWarn("webhook index backfill: history load failed", "key", key, "error", err.Error())
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

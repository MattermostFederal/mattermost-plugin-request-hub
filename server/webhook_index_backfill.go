package main

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
)

// backfillWebhookIndexFromHistory writes webhook_index_* rows for standalone
// webhooks that were approved before the index existed. Best-effort: skips
// exercise hooks, already-indexed hooks, and histories without a hook_id.
func (p *Plugin) backfillWebhookIndexFromHistory() {
	for page := 0; ; page++ {
		keys, appErr := p.API.KVList(page, webhookIndexPageSize)
		if appErr != nil {
			p.API.LogWarn("webhook index backfill: KVList failed", "error", appErr.Error())
			return
		}
		for _, key := range keys {
			if !strings.HasPrefix(key, kvHistoryPrefix) {
				continue
			}
			h, err := p.loadHistory(strings.TrimPrefix(key, kvHistoryPrefix))
			if err != nil || h == nil || h.Kind != pendingKindWebhook || h.Status != historyApproved {
				continue
			}
			hookID := webhookHookIDFromHistory(h)
			if hookID == "" {
				continue
			}
			existing, loadErr := p.loadWebhookIndex(hookID)
			if loadErr != nil || existing != nil {
				continue
			}
			var snap webhookRequest
			if len(h.Request) > 0 {
				_ = json.Unmarshal(h.Request, &snap)
			}
			if snap.Exercise {
				continue
			}
			if snap.RequesterID == "" {
				snap.RequesterID = h.RequesterID
			}
			if snap.ID == "" {
				snap.ID = h.RequestID
			}
			p.writeWebhookIndex(&snap, hookID, "")
		}
		if len(keys) < webhookIndexPageSize {
			return
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

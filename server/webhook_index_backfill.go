package main

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"

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
	// Reading a hook's live owner needs webhook-management rights, so the
	// whole run borrows one System Admin's session.
	adminUserID := p.backfillAdminUserID()
	for page := 0; ; page++ {
		keys, listErr := p.API.KVList(page, webhookIndexPageSize)
		if listErr != nil {
			p.API.LogWarn("webhook index backfill: KVList failed", "error", listErr.Error())
			return
		}
		p.backfillWebhookIndexKeys(keys, adminUserID)
		if len(keys) < webhookIndexPageSize {
			return
		}
	}
}

// backfillAdminUserID resolves an active System Admin whose session can read
// live webhooks, on the first hook that actually needs one. Most activations
// find nothing to index, and listing admins every time would be wasted work.
// Empty when none can be listed; the run then writes nothing.
func (p *Plugin) backfillAdminUserID() func() string {
	var (
		once sync.Once
		id   string
	)
	return func() string {
		once.Do(func() {
			admins, appErr := p.API.GetUsers(&model.UserGetOptions{Role: model.SystemAdminRoleId, Active: true, Page: 0, PerPage: 1})
			if appErr != nil || len(admins) == 0 {
				p.API.LogWarn("webhook index backfill: no System Admin available to read live webhook owners")
				return
			}
			id = admins[0].Id
		})
		return id
	}
}

// backfillWebhookIndexKeys indexes the approved webhook requests among keys.
func (p *Plugin) backfillWebhookIndexKeys(keys []string, adminUserID func() string) {
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
		// Owner and channel may both have moved on since the request was
		// approved, and the index row is what grants transfer rights: the
		// owner decides who may file, the channel decides who may step in
		// once the hook is orphaned. A historical copy of either would hand
		// those rights to the wrong people, so skip the hook and let a
		// later run pick it up.
		owner, channelID, scopeKnown := p.liveWebhookScope(snap.WebhookType, hookID, adminUserID())
		if !scopeKnown {
			p.API.LogWarn("webhook index backfill: skipping a hook whose live owner or channel could not be read", "request_id", h.RequestID)
			continue
		}
		entry := webhookIndexEntry{
			HookID:      hookID,
			WebhookType: snap.WebhookType,
			ChannelID:   channelID,
			TeamID:      snap.TeamID,
			DisplayName: snap.DisplayName,
			Description: strings.TrimSpace(snap.Description),
			CallbackURL: strings.TrimSpace(snap.CallbackURL),
			OwnerUserID: owner,
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

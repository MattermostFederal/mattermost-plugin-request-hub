package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
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
//
// Outgoing approvals record their hook ID. Incoming ones cannot — the ID is
// the secret URL — so those are identified against the live hooks instead
// (see matchIncomingWebhookForBackfill).
func (p *Plugin) backfillWebhookIndexFromHistory() {
	// Reading a hook's live owner needs webhook-management rights, so the
	// whole run borrows one System Admin's session.
	adminUserID := p.backfillAdminUserID()
	hooks := p.backfillIncomingWebhookLister(adminUserID)
	for page := 0; ; page++ {
		keys, listErr := p.API.KVList(page, webhookIndexPageSize)
		if listErr != nil {
			p.API.LogWarn("webhook index backfill: KVList failed", "error", listErr.Error())
			return
		}
		p.backfillWebhookIndexKeys(keys, adminUserID, hooks)
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
func (p *Plugin) backfillWebhookIndexKeys(keys []string, adminUserID func() string, hooks incomingWebhookLister) {
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
		hookID := webhookHookIDFromHistory(h)
		if hookID == "" && snap.WebhookType == webhookTypeIncoming {
			// An incoming hook's ID is its secret URL, so the approval
			// deliberately recorded a transfer handle instead and there is
			// nothing in history to read. Find the hook itself instead.
			hookID = p.matchIncomingWebhookForBackfill(&snap, h.RequestID, hooks)
		}
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

// incomingWebhookLister lists a team's live incoming hooks once per run.
// ok is false when the read failed; the caller then indexes nothing rather
// than guessing from a partial list.
type incomingWebhookLister func(teamID string) (hooks []*model.IncomingWebhook, ok bool)

// backfillIncomingWebhookLister caches each team's live incoming hooks for
// the length of one backfill run. The list is only fetched for a team that
// actually has an unindexed incoming hook to identify.
func (p *Plugin) backfillIncomingWebhookLister(adminUserID func() string) incomingWebhookLister {
	type teamHooks struct {
		hooks []*model.IncomingWebhook
		ok    bool
	}
	cache := map[string]teamHooks{}
	return func(teamID string) ([]*model.IncomingWebhook, bool) {
		if cached, seen := cache[teamID]; seen {
			return cached.hooks, cached.ok
		}
		hooks, err := p.liveIncomingWebhooksForTeam(teamID, adminUserID())
		if err != nil {
			p.API.LogWarn("webhook index backfill: could not list a team's incoming webhooks", "team_id", teamID, "error", err.Error())
		}
		cache[teamID] = teamHooks{hooks: hooks, ok: err == nil}
		return hooks, err == nil
	}
}

// matchIncomingWebhookForBackfill identifies the live incoming hook an
// approved request created, by the fields the create call set from that
// request: team, channel, owner and display name. A request that matches
// more than one hook is skipped — writing the index row for the wrong hook
// would hand its transfer rights to someone who never owned it, and there
// is nothing to undo it with. Nothing secret is read or recorded: the hook
// ID that goes into the index is the index's own key everywhere else.
func (p *Plugin) matchIncomingWebhookForBackfill(snap *webhookRequest, requestID string, hooks incomingWebhookLister) string {
	channelID := strings.TrimSpace(snap.ChannelID)
	displayName := strings.TrimSpace(snap.DisplayName)
	if snap.TeamID == "" || channelID == "" || displayName == "" || snap.RequesterID == "" {
		return ""
	}
	live, ok := hooks(snap.TeamID)
	if !ok {
		return ""
	}
	var matched string
	for _, hook := range live {
		if hook == nil || hook.ChannelId != channelID || hook.UserId != snap.RequesterID ||
			strings.TrimSpace(hook.DisplayName) != displayName {
			continue
		}
		// A hook that is already indexed belongs to whichever request put
		// it there, so it is not a candidate for this one.
		if entry, err := p.loadWebhookIndex(hook.Id); err != nil || entry != nil {
			continue
		}
		if matched != "" {
			p.API.LogWarn("webhook index backfill: more than one incoming webhook matches an approved request, skipping it",
				"request_id", requestID, "channel_id", channelID)
			return ""
		}
		matched = hook.Id
	}
	return matched
}

// liveIncomingWebhooksForTeam reads a team's incoming hooks through a System
// Admin's session.
func (p *Plugin) liveIncomingWebhooksForTeam(teamID, adminUserID string) ([]*model.IncomingWebhook, error) {
	if p.listIncomingWebhooksFn != nil {
		return p.listIncomingWebhooksFn(teamID, adminUserID)
	}
	if adminUserID == "" {
		return nil, errors.New("no System Admin session is available to list incoming webhooks")
	}
	client, cleanup, err := p.transferClient(adminUserID)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	ctx := context.Background()
	var all []*model.IncomingWebhook
	for page := 0; ; page++ {
		hooks, _, apiErr := client.GetIncomingWebhooksForTeam(ctx, teamID, page, webhookIndexPageSize, "")
		if apiErr != nil {
			return nil, errors.Wrap(apiErr, "failed to list incoming webhooks")
		}
		all = append(all, hooks...)
		if len(hooks) < webhookIndexPageSize {
			return all, nil
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

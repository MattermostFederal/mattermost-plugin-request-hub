package main

import (
	"fmt"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/pluginapi/cluster"
)

// Orphaned-owner detection. A daily cluster job scans the bot and webhook
// indexes; any asset whose owner's account is gone or deactivated is an
// orphan. For each, it posts a deduped card to the audit channel with
// Transfer and Retire buttons. Whoever clicks becomes the transfer ticket's
// requester "on behalf of" the deactivated owner (FromOrphanCard). There is
// no auto-ticket timer — a human always decides. Secrets only ever reach a
// new owner after two-step approval.

const (
	orphanOwnerJobKey    = "orphan_owner_scan"
	orphanOwnerJobPeriod = 24 * time.Hour
	orphanNoticePrefix   = "orphan_notice_"
)

func orphanNoticeKey(assetType, assetID string) string {
	return orphanNoticePrefix + assetType + "_" + assetID
}

// botIndexOwner is the bot's effective owner. Older index rows left owner
// empty when the owner was the requester, so fall back to the requester.
func botIndexOwner(e botIndexEntry) string {
	if e.OwnerUserID != "" {
		return e.OwnerUserID
	}
	return e.RequesterID
}

func (p *Plugin) startOrphanOwnerJob() {
	job, err := cluster.Schedule(p.API, orphanOwnerJobKey, cluster.MakeWaitForInterval(orphanOwnerJobPeriod), func() {
		defer p.logRecoveredPanic("orphan owner scan")
		p.scanOrphanedOwners()
	})
	if err != nil {
		p.API.LogError("failed to schedule orphan owner scan", "error", err.Error())
		return
	}
	p.orphanOwnerJob = job
}

func (p *Plugin) stopOrphanOwnerJob() {
	if p.orphanOwnerJob == nil {
		return
	}
	if err := p.orphanOwnerJob.Close(); err != nil {
		p.API.LogWarn("failed to stop orphan owner scan", "error", err.Error())
	}
	p.orphanOwnerJob = nil
}

// scanOrphanedOwners posts an audit card for every RH-managed asset whose
// owner is deactivated and that has no card yet.
func (p *Plugin) scanOrphanedOwners() {
	bots, err := p.listBotIndex()
	if err != nil {
		p.API.LogError("orphan scan: failed to list bot index", "error", err.Error())
	}
	for _, e := range bots {
		owner := botIndexOwner(e)
		if e.RetiredAt != 0 || p.userActive(owner) {
			continue
		}
		p.noticeOrphan("bot", e.BotUserID, "@"+e.Username, owner)
	}

	hooks, err := p.listWebhookIndex()
	if err != nil {
		p.API.LogError("orphan scan: failed to list webhook index", "error", err.Error())
	}
	for _, e := range hooks {
		if e.RetiredAt != 0 || p.userActive(e.OwnerUserID) {
			continue
		}
		label := e.DisplayName
		if label == "" {
			label = e.HookID
		}
		p.noticeOrphan("webhook", e.HookID, e.WebhookType+" webhook "+label, e.OwnerUserID)
	}
}

// noticeOrphan posts one orphan card, deduped per asset. A notice KV marker
// keyed by asset means a card already exists, so we skip it; the marker is
// cleared when the asset is transferred or retired.
func (p *Plugin) noticeOrphan(assetType, assetID, displayName, ownerID string) {
	key := orphanNoticeKey(assetType, assetID)
	if existing, appErr := p.API.KVGet(key); appErr == nil && existing != nil {
		return
	}
	postID := p.postOrphanCard(assetType, assetID, displayName, ownerID)
	if postID == "" {
		return
	}
	if appErr := p.API.KVSet(key, []byte(postID)); appErr != nil {
		p.API.LogWarn("failed to record orphan notice", "asset_id", assetID, "error", appErr.Error())
	}
	p.API.LogInfo("posted orphaned-owner card", "asset_type", assetType, "asset_id", assetID, "owner_id", ownerID)
}

// clearOrphanNotice removes the dedupe marker and best-effort removes the
// audit card once the asset is dealt with.
func (p *Plugin) clearOrphanNotice(assetType, assetID string) {
	key := orphanNoticeKey(assetType, assetID)
	postID, appErr := p.API.KVGet(key)
	if appErr != nil || postID == nil {
		return
	}
	if delErr := p.API.DeletePost(string(postID)); delErr != nil {
		p.API.LogWarn("failed to delete orphan card", "asset_id", assetID, "error", delErr.Error())
	}
	if delErr := p.API.KVDelete(key); delErr != nil {
		p.API.LogWarn("failed to clear orphan notice", "asset_id", assetID, "error", delErr.Error())
	}
}

// postOrphanCard posts the Transfer / Retire card to the audit channel.
func (p *Plugin) postOrphanCard(assetType, assetID, displayName, ownerID string) string {
	ch := p.ensureAuditChannel()
	if ch == nil {
		return ""
	}
	siteURL := "/plugins/" + manifest.Id
	ctx := map[string]any{"asset_type": assetType, "asset_id": assetID}
	att := &model.MessageAttachment{
		Title: "Orphaned " + assetType + " — owner deactivated",
		Color: colorPending,
		Text:  fmt.Sprintf("**%s** has no active owner (%s). Transfer it to a new owner or retire it. Both route through security + system approval; secrets only ever go to the new owner after approval.", displayName, p.mentionList([]string{ownerID})),
		Fields: []*model.MessageAttachmentField{
			{Title: "Asset", Value: displayName, Short: true},
			{Title: "Previous owner", Value: p.mentionList([]string{ownerID}), Short: true},
		},
		Actions: []*model.PostAction{
			{
				Id: "orphan_transfer", Name: "Transfer", Type: model.PostActionTypeButton, Style: "primary",
				Integration: &model.PostActionIntegration{URL: siteURL + routeOrphanTransfer, Context: ctx},
			},
			{
				Id: "orphan_retire", Name: "Retire", Type: model.PostActionTypeButton, Style: "danger",
				Integration: &model.PostActionIntegration{URL: siteURL + routeOrphanRetire, Context: ctx},
			},
		},
	}
	post := &model.Post{UserId: p.botUserID, ChannelId: ch.Id, Message: p.systemAdminMentions() + " — an RH-managed asset lost its owner."}
	model.ParseMessageAttachment(post, []*model.MessageAttachment{att})
	created, appErr := p.API.CreatePost(post)
	if appErr != nil {
		p.API.LogWarn("failed to post orphan card", "asset_id", assetID, "error", appErr.Error())
		return ""
	}
	return created.Id
}

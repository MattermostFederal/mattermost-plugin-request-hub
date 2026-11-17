package main

// Orphan cleanup helpers. Mattermost core already notifies System Admins
// (self-DM) when a bot owner is deactivated and optionally disables those
// bots. Request Hub does not post orphan nudge cards; admins open Transfer /
// Retire from the hub when they need a governed handoff. clearOrphanNotice
// still drops any leftover notice KV / audit card from earlier builds.

const orphanNoticePrefix = "orphan_notice_"

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

// clearOrphanNotice removes a leftover orphan-notice KV marker and best-effort
// deletes the linked audit card (from builds that posted those cards).
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

package main

import (
	"encoding/json"
	"slices"

	"github.com/mattermost/mattermost/server/public/model"
)

// kvTAVisibilitySeededPrefix stores, per approval channel, the user IDs the
// plugin added for Team Admin visibility. Eviction only removes those IDs so
// hand-added observers and cross-team approvers are not kicked.
const kvTAVisibilitySeededPrefix = "ta_vis_seeded_v1_"

const taVisibilitySeedCASAttempts = 8

func taVisibilitySeededKey(channelID string) string {
	return kvTAVisibilitySeededPrefix + channelID
}

// markTAVisibilitySeeded records userID as plugin-seeded on channelID and
// reports whether the ledger now holds the entry.
func (p *Plugin) markTAVisibilitySeeded(channelID, userID string) bool {
	if channelID == "" || userID == "" {
		return false
	}
	key := taVisibilitySeededKey(channelID)
	for range taVisibilitySeedCASAttempts {
		raw, appErr := p.API.KVGet(key)
		if appErr != nil {
			p.API.LogWarn("ta visibility seed: failed to load", "channel_id", channelID, "error", appErr.Error())
			return false
		}
		var ids []string
		if raw != nil {
			if err := json.Unmarshal(raw, &ids); err != nil {
				// Refuse to overwrite an unreadable ledger — rewriting with a
				// single ID would drop every other seeded entry and leave those
				// users unevictable.
				p.API.LogWarn("ta visibility seed: refusing to overwrite unreadable ledger",
					"channel_id", channelID, "user_id", userID, "error", err.Error())
				return false
			}
		}
		if slices.Contains(ids, userID) {
			return true
		}
		next := append(slices.Clone(ids), userID)
		data, err := json.Marshal(next)
		if err != nil {
			return false
		}
		ok, casErr := p.API.KVCompareAndSet(key, raw, data)
		if casErr != nil {
			p.API.LogWarn("ta visibility seed: failed to mark", "channel_id", channelID, "user_id", userID, "error", casErr.Error())
			return false
		}
		if ok {
			return true
		}
	}
	p.API.LogWarn("ta visibility seed: mark exhausted CAS retries",
		"channel_id", channelID, "user_id", userID, "attempts", taVisibilitySeedCASAttempts)
	return false
}

// recordSeededMember marks a membership the plugin just added. When the
// ledger write fails the add is rolled back: an unrecorded member is treated
// as a hand-added observer and is never evicted on demotion.
func (p *Plugin) recordSeededMember(channelID, userID string) bool {
	if p.markTAVisibilitySeeded(channelID, userID) {
		return true
	}
	if delErr := p.API.DeleteChannelMember(channelID, userID); delErr != nil && !isNotFound(delErr) {
		p.API.LogError("Request Hub added a user to an approval channel but could not record it or undo it. Remove the user from the channel if they should not see approval requests.",
			"channel_id", channelID, "user_id", userID, "error", delErr.Error())
		return false
	}
	p.API.LogWarn("ta visibility seed: rolled back an add that could not be recorded; a later sync retries",
		"channel_id", channelID, "user_id", userID)
	return false
}

// ensureSeededMember makes userID a member of channelID and reports whether
// they are one. Only a membership the plugin adds is recorded: an existing
// member may have been added by hand and must not become evictable.
func (p *Plugin) ensureSeededMember(channelID, userID string) bool {
	_, memErr := p.API.GetChannelMember(channelID, userID)
	if memErr == nil {
		return true
	}
	if !isNotFound(memErr) {
		p.API.LogWarn("approval channel: membership lookup failed; skipping add",
			"channel_id", channelID, "user_id", userID, "error", memErr.Error())
		return false
	}
	if _, addErr := p.API.AddChannelMember(channelID, userID); addErr != nil {
		p.API.LogWarn("approval channel: failed to add channel member",
			"channel_id", channelID, "user_id", userID, "error", addErr.Error())
		return false
	}
	return p.recordSeededMember(channelID, userID)
}

// evictSeededMember removes a plugin-seeded member, then clears the ledger
// entry. The entry is kept until the delete succeeds, so a failed delete or a
// crash leaves the user recorded and a later sweep retries; a failed clear
// only leaves an entry for someone no longer in the channel.
//
// A user who is a System Admin, or for whom stillQualifies reports true,
// after the clear may have been re-added and recorded by a concurrent seed
// before the clear removed that record. An existing member is re-recorded,
// and a missing one is added back. When the role lookup fails, an existing
// member is still re-recorded so their seat stays revocable, but nobody is
// added back. Returns whether the user ended up removed.
func (p *Plugin) evictSeededMember(ch *model.Channel, userID string, stillQualifies func() bool) (bool, *model.AppError) {
	if delErr := p.API.DeleteChannelMember(ch.Id, userID); delErr != nil && !isNotFound(delErr) {
		return false, delErr
	}
	if !p.clearTAVisibilitySeeded(ch.Id, userID) {
		p.API.LogWarn("ta visibility seed: removed a member but could not clear their ledger entry; if they are added back by hand, the next sweep may remove them",
			"channel_id", ch.Id, "user_id", userID)
	}
	sysAdmin, roleKnown := p.lookupSystemAdmin(userID)
	qualifies := sysAdmin || (stillQualifies != nil && stillQualifies())
	if roleKnown && !qualifies {
		return true, nil
	}
	_, memErr := p.API.GetChannelMember(ch.Id, userID)
	switch {
	case memErr == nil:
		p.recordSeededMember(ch.Id, userID)
		return false, nil
	case isNotFound(memErr):
		switch {
		case sysAdmin:
			p.ensureSeededMember(ch.Id, userID)
		case qualifies:
			p.seedTAVisibility(ch, userID)
		}
		return !qualifies, nil
	default:
		p.API.LogWarn("ta visibility seed: membership lookup failed after eviction; the next sync re-seeds",
			"channel_id", ch.Id, "user_id", userID, "error", memErr.Error())
		return false, nil
	}
}

// lookupSystemAdmin reports whether userID is an active System Admin, and
// whether the lookup succeeded.
func (p *Plugin) lookupSystemAdmin(userID string) (isAdmin, known bool) {
	user, appErr := p.API.GetUser(userID)
	if appErr != nil || user == nil {
		return false, false
	}
	return user.DeleteAt == 0 && user.IsSystemAdmin(), true
}

// clearTAVisibilitySeeded removes userID from channelID's ledger and reports
// whether the ledger no longer holds the entry (true when it was absent).
func (p *Plugin) clearTAVisibilitySeeded(channelID, userID string) bool {
	if channelID == "" || userID == "" {
		return true
	}
	key := taVisibilitySeededKey(channelID)
	for range taVisibilitySeedCASAttempts {
		raw, appErr := p.API.KVGet(key)
		if appErr != nil {
			p.API.LogWarn("ta visibility seed: failed to load for clear",
				"channel_id", channelID, "user_id", userID, "error", appErr.Error())
			return false
		}
		if raw == nil {
			return true
		}
		var ids []string
		if err := json.Unmarshal(raw, &ids); err != nil {
			p.API.LogWarn("ta visibility seed: refusing to clear unreadable ledger",
				"channel_id", channelID, "user_id", userID, "error", err.Error())
			return false
		}
		idx := slices.Index(ids, userID)
		if idx < 0 {
			return true
		}
		next := slices.Delete(slices.Clone(ids), idx, idx+1)
		data, err := json.Marshal(next)
		if err != nil {
			return false
		}
		ok, casErr := p.API.KVCompareAndSet(key, raw, data)
		if casErr != nil {
			p.API.LogWarn("ta visibility seed: failed to clear",
				"channel_id", channelID, "user_id", userID, "error", casErr.Error())
			return false
		}
		if ok {
			return true
		}
	}
	p.API.LogWarn("ta visibility seed: clear exhausted CAS retries",
		"channel_id", channelID, "user_id", userID, "attempts", taVisibilitySeedCASAttempts)
	return false
}

func (p *Plugin) isTAVisibilitySeeded(channelID, userID string) bool {
	_, ok := p.loadTAVisibilitySeededSet(channelID)[userID]
	return ok
}

// loadTAVisibilitySeededSet returns the seeded user IDs for channelID as a
// set. Callers that check many members should load once instead of KVGet
// per user.
func (p *Plugin) loadTAVisibilitySeededSet(channelID string) map[string]struct{} {
	out := map[string]struct{}{}
	if channelID == "" {
		return out
	}
	raw, appErr := p.API.KVGet(taVisibilitySeededKey(channelID))
	if appErr != nil {
		p.API.LogWarn("TA visibility seed: failed to load ledger",
			"channel_id", channelID, "error", appErr.Error())
		return out
	}
	if raw == nil {
		return out
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		p.API.LogWarn("TA visibility seed: unreadable ledger",
			"channel_id", channelID, "error", err.Error())
		return out
	}
	for _, id := range ids {
		if id != "" {
			out[id] = struct{}{}
		}
	}
	return out
}

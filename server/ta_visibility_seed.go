package main

import (
	"encoding/json"
	"slices"
)

// kvTAVisibilitySeededPrefix stores, per approval channel, the user IDs the
// plugin added for Team Admin visibility. Eviction only removes those IDs so
// hand-added observers and cross-team approvers are not kicked.
const kvTAVisibilitySeededPrefix = "ta_vis_seeded_v1_"

const taVisibilitySeedCASAttempts = 8

func taVisibilitySeededKey(channelID string) string {
	return kvTAVisibilitySeededPrefix + channelID
}

func (p *Plugin) markTAVisibilitySeeded(channelID, userID string) {
	if channelID == "" || userID == "" {
		return
	}
	key := taVisibilitySeededKey(channelID)
	for range taVisibilitySeedCASAttempts {
		raw, appErr := p.API.KVGet(key)
		if appErr != nil {
			p.API.LogWarn("ta visibility seed: failed to load", "channel_id", channelID, "error", appErr.Error())
			return
		}
		var ids []string
		if raw != nil {
			if err := json.Unmarshal(raw, &ids); err != nil {
				// Refuse to overwrite an unreadable ledger — rewriting with a
				// single ID would drop every other seeded entry and leave those
				// users unevictable.
				p.API.LogWarn("ta visibility seed: refusing to overwrite unreadable ledger",
					"channel_id", channelID, "user_id", userID, "error", err.Error())
				return
			}
		}
		if slices.Contains(ids, userID) {
			return
		}
		next := append(slices.Clone(ids), userID)
		data, err := json.Marshal(next)
		if err != nil {
			return
		}
		ok, casErr := p.API.KVCompareAndSet(key, raw, data)
		if casErr != nil {
			p.API.LogWarn("ta visibility seed: failed to mark", "channel_id", channelID, "user_id", userID, "error", casErr.Error())
			return
		}
		if ok {
			return
		}
	}
	p.API.LogWarn("ta visibility seed: mark exhausted CAS retries",
		"channel_id", channelID, "user_id", userID, "attempts", taVisibilitySeedCASAttempts)
}

func (p *Plugin) clearTAVisibilitySeeded(channelID, userID string) {
	if channelID == "" || userID == "" {
		return
	}
	key := taVisibilitySeededKey(channelID)
	for range taVisibilitySeedCASAttempts {
		raw, appErr := p.API.KVGet(key)
		if appErr != nil {
			p.API.LogWarn("ta visibility seed: failed to load for clear",
				"channel_id", channelID, "user_id", userID, "error", appErr.Error())
			return
		}
		if raw == nil {
			return
		}
		var ids []string
		if err := json.Unmarshal(raw, &ids); err != nil {
			p.API.LogWarn("ta visibility seed: refusing to clear unreadable ledger",
				"channel_id", channelID, "user_id", userID, "error", err.Error())
			return
		}
		idx := slices.Index(ids, userID)
		if idx < 0 {
			return
		}
		next := slices.Delete(slices.Clone(ids), idx, idx+1)
		data, err := json.Marshal(next)
		if err != nil {
			return
		}
		ok, casErr := p.API.KVCompareAndSet(key, raw, data)
		if casErr != nil {
			p.API.LogWarn("ta visibility seed: failed to clear",
				"channel_id", channelID, "user_id", userID, "error", casErr.Error())
			return
		}
		if ok {
			return
		}
	}
	p.API.LogWarn("ta visibility seed: clear exhausted CAS retries",
		"channel_id", channelID, "user_id", userID, "attempts", taVisibilitySeedCASAttempts)
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

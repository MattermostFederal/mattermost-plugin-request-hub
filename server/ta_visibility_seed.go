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
				ids = nil
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
}

func (p *Plugin) clearTAVisibilitySeeded(channelID, userID string) {
	if channelID == "" || userID == "" {
		return
	}
	key := taVisibilitySeededKey(channelID)
	for range taVisibilitySeedCASAttempts {
		raw, appErr := p.API.KVGet(key)
		if appErr != nil || raw == nil {
			return
		}
		var ids []string
		if err := json.Unmarshal(raw, &ids); err != nil {
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
			return
		}
		if ok {
			return
		}
	}
}

func (p *Plugin) isTAVisibilitySeeded(channelID, userID string) bool {
	if channelID == "" || userID == "" {
		return false
	}
	raw, appErr := p.API.KVGet(taVisibilitySeededKey(channelID))
	if appErr != nil || raw == nil {
		return false
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return false
	}
	return slices.Contains(ids, userID)
}

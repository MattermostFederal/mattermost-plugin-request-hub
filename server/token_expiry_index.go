package main

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/pkg/errors"
)

// kvTokenExpiryIndex holds bot_token_expiry_* keys so the hourly reminder job
// does not scan the entire plugin KV store (histories, anchors, …).
const kvTokenExpiryIndex = "bot_token_expiry_index_v1"

const tokenExpiryIndexCASAttempts = 8

// listTokenExpiryKeys returns tracked token-expiry KV keys from the index,
// rebuilding from a full scan when the index is missing.
func (p *Plugin) listTokenExpiryKeys() ([]string, error) {
	if keys, ok := p.loadTokenExpiryIndex(); ok {
		return keys, nil
	}
	keys, err := p.scanTokenExpiryKeys()
	if err != nil {
		return nil, err
	}
	if saveErr := p.saveTokenExpiryIndex(keys); saveErr != nil {
		p.API.LogWarn("failed to persist token-expiry index; list still returned",
			"error", saveErr.Error(), "count", len(keys))
	}
	return keys, nil
}

func (p *Plugin) scanTokenExpiryKeys() ([]string, error) {
	const perPage = 200
	var keys []string
	for page := 0; ; page++ {
		batch, appErr := p.API.KVList(page, perPage)
		if appErr != nil {
			return nil, errors.Wrap(appErr, "failed to list bot token expiries")
		}
		for _, key := range batch {
			if strings.HasPrefix(key, botTokenExpiryKeyPrefix) {
				keys = append(keys, key)
			}
		}
		if len(batch) < perPage {
			return keys, nil
		}
	}
}

func (p *Plugin) loadTokenExpiryIndex() ([]string, bool) {
	raw, appErr := p.API.KVGet(kvTokenExpiryIndex)
	if appErr != nil || raw == nil {
		return nil, false
	}
	var keys []string
	if err := json.Unmarshal(raw, &keys); err != nil {
		p.API.LogWarn("token-expiry index unreadable; will rebuild", "error", err.Error())
		return nil, false
	}
	return keys, true
}

func (p *Plugin) saveTokenExpiryIndex(keys []string) error {
	if keys == nil {
		keys = []string{}
	}
	data, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	if appErr := p.API.KVSet(kvTokenExpiryIndex, data); appErr != nil {
		return appErr
	}
	return nil
}

func (p *Plugin) trackTokenExpiryKey(key string) {
	if key == "" {
		return
	}
	for range tokenExpiryIndexCASAttempts {
		raw, appErr := p.API.KVGet(kvTokenExpiryIndex)
		if appErr != nil {
			p.API.LogWarn("token-expiry index: failed to load for add", "key", key, "error", appErr.Error())
			return
		}
		var keys []string
		if raw != nil {
			if err := json.Unmarshal(raw, &keys); err != nil {
				p.rebuildTokenExpiryIndex()
				continue
			}
		}
		if slices.Contains(keys, key) {
			return
		}
		next := append(slices.Clone(keys), key)
		data, err := json.Marshal(next)
		if err != nil {
			return
		}
		ok, casErr := p.API.KVCompareAndSet(kvTokenExpiryIndex, raw, data)
		if casErr != nil {
			p.API.LogWarn("token-expiry index: failed to add key", "key", key, "error", casErr.Error())
			return
		}
		if ok {
			return
		}
	}
	p.API.LogWarn("token-expiry index: gave up adding key after CAS contention", "key", key)
}

func (p *Plugin) untrackTokenExpiryKey(key string) {
	if key == "" {
		return
	}
	for range tokenExpiryIndexCASAttempts {
		raw, appErr := p.API.KVGet(kvTokenExpiryIndex)
		if appErr != nil {
			p.API.LogWarn("token-expiry index: failed to load for remove", "key", key, "error", appErr.Error())
			return
		}
		if raw == nil {
			return
		}
		var keys []string
		if err := json.Unmarshal(raw, &keys); err != nil {
			return
		}
		idx := slices.Index(keys, key)
		if idx < 0 {
			return
		}
		next := slices.Delete(slices.Clone(keys), idx, idx+1)
		data, err := json.Marshal(next)
		if err != nil {
			return
		}
		ok, casErr := p.API.KVCompareAndSet(kvTokenExpiryIndex, raw, data)
		if casErr != nil {
			p.API.LogWarn("token-expiry index: failed to remove key", "key", key, "error", casErr.Error())
			return
		}
		if ok {
			return
		}
	}
	p.API.LogWarn("token-expiry index: gave up removing key after CAS contention", "key", key)
}

func (p *Plugin) rebuildTokenExpiryIndex() {
	keys, err := p.scanTokenExpiryKeys()
	if err != nil {
		p.API.LogWarn("token-expiry index: rebuild scan failed", "error", err.Error())
		return
	}
	if err := p.saveTokenExpiryIndex(keys); err != nil {
		p.API.LogWarn("token-expiry index: rebuild save failed", "error", err.Error())
		return
	}
	p.API.LogInfo("token-expiry index: rebuilt", "count", len(keys))
}

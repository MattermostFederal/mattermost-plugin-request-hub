package main

import (
	"slices"
	"strings"

	"github.com/pkg/errors"
)

// kvTokenExpiryIndex holds bot_token_expiry_* keys so the hourly reminder job
// does not scan the entire plugin KV store (histories, anchors, …).
const kvTokenExpiryIndex = "bot_token_expiry_index_v1" //nolint:gosec // G101: a KV key, not a credential

const tokenExpiryIndexCASAttempts = 8

// listTokenExpiryKeys returns tracked token-expiry KV keys from the index,
// rebuilding from a full scan when the index is missing.
func (p *Plugin) listTokenExpiryKeys() ([]string, error) {
	if keys, ok := p.loadTokenExpiryIndex(); ok {
		return keys, nil
	}
	prev, _ := p.API.KVGet(kvTokenExpiryIndex)
	keys, err := p.scanTokenExpiryKeys()
	if err != nil {
		return nil, err
	}
	if saveErr := p.persistTokenExpiryIndexFromScan(prev, keys); saveErr != nil {
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
			// Index key shares the bot_token_expiry_ prefix; never treat it as a token.
			if key != kvTokenExpiryIndex && strings.HasPrefix(key, botTokenExpiryKeyPrefix) {
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
	keys, complete, ok := parseKeyIndex(raw)
	if !ok {
		p.API.LogWarn("token-expiry index unreadable; will rebuild")
		return nil, false
	}
	if !complete {
		return nil, false
	}
	return keys, true
}

// persistTokenExpiryIndexFromScan CAS-writes a scan result against prev, then
// unions with the live index on conflict (same race as open-request index).
func (p *Plugin) persistTokenExpiryIndexFromScan(prev []byte, scanned []string) error {
	keys := scanned
	if keys == nil {
		keys = []string{}
	}
	expected := prev
	for range tokenExpiryIndexCASAttempts {
		data, err := marshalCompleteKeyIndex(keys)
		if err != nil {
			return err
		}
		ok, casErr := p.API.KVCompareAndSet(kvTokenExpiryIndex, expected, data)
		if casErr != nil {
			return casErr
		}
		if ok {
			return nil
		}
		raw, appErr := p.API.KVGet(kvTokenExpiryIndex)
		if appErr != nil {
			return appErr
		}
		var current []string
		if raw != nil {
			parsed, _, parseOK := parseKeyIndex(raw)
			if !parseOK {
				expected = raw
				continue
			}
			current = parsed
		}
		keys = unionStringKeys(scanned, current)
		expected = raw
	}
	return errors.New("token-expiry index: CAS contention persisting scan")
}

// invalidateTokenExpiryIndex deletes the index so the next list rebuilds from
// a full scan. Same rationale as invalidateOpenRequestIndex.
func (p *Plugin) invalidateTokenExpiryIndex() {
	if appErr := p.API.KVDelete(kvTokenExpiryIndex); appErr != nil {
		p.API.LogWarn("token-expiry index: failed to invalidate", "error", appErr.Error())
	}
}

func (p *Plugin) trackTokenExpiryKey(key string) {
	if key == "" {
		return
	}
	for range tokenExpiryIndexCASAttempts {
		raw, appErr := p.API.KVGet(kvTokenExpiryIndex)
		if appErr != nil {
			p.API.LogWarn("token-expiry index: failed to load for add", "key", key, "error", appErr.Error())
			p.invalidateTokenExpiryIndex()
			return
		}
		if raw == nil {
			data, err := marshalPartialKeyIndex([]string{key})
			if err != nil {
				p.invalidateTokenExpiryIndex()
				return
			}
			ok, casErr := p.API.KVCompareAndSet(kvTokenExpiryIndex, nil, data)
			if casErr != nil {
				p.API.LogWarn("token-expiry index: failed to add key", "key", key, "error", casErr.Error())
				p.invalidateTokenExpiryIndex()
				return
			}
			if ok {
				return
			}
			continue
		}
		keys, complete, ok := parseKeyIndex(raw)
		if !ok {
			p.invalidateTokenExpiryIndex()
			return
		}
		if slices.Contains(keys, key) {
			return
		}
		next := append(slices.Clone(keys), key)
		var data []byte
		var err error
		if complete {
			data, err = marshalCompleteKeyIndex(next)
		} else {
			data, err = marshalPartialKeyIndex(next)
		}
		if err != nil {
			p.invalidateTokenExpiryIndex()
			return
		}
		casOK, casErr := p.API.KVCompareAndSet(kvTokenExpiryIndex, raw, data)
		if casErr != nil {
			p.API.LogWarn("token-expiry index: failed to add key", "key", key, "error", casErr.Error())
			p.invalidateTokenExpiryIndex()
			return
		}
		if casOK {
			return
		}
	}
	p.API.LogWarn("token-expiry index: gave up adding key after CAS contention", "key", key)
	p.invalidateTokenExpiryIndex()
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
		keys, complete, ok := parseKeyIndex(raw)
		if !ok {
			return
		}
		idx := slices.Index(keys, key)
		if idx < 0 {
			return
		}
		next := slices.Delete(slices.Clone(keys), idx, idx+1)
		var data []byte
		var err error
		if complete {
			data, err = marshalCompleteKeyIndex(next)
		} else {
			data, err = marshalPartialKeyIndex(next)
		}
		if err != nil {
			return
		}
		casOK, casErr := p.API.KVCompareAndSet(kvTokenExpiryIndex, raw, data)
		if casErr != nil {
			p.API.LogWarn("token-expiry index: failed to remove key", "key", key, "error", casErr.Error())
			return
		}
		if casOK {
			return
		}
	}
	p.API.LogWarn("token-expiry index: gave up removing key after CAS contention", "key", key)
}

func (p *Plugin) rebuildTokenExpiryIndex() {
	prev, _ := p.API.KVGet(kvTokenExpiryIndex)
	keys, err := p.scanTokenExpiryKeys()
	if err != nil {
		p.API.LogWarn("token-expiry index: rebuild scan failed", "error", err.Error())
		return
	}
	if err := p.persistTokenExpiryIndexFromScan(prev, keys); err != nil {
		p.API.LogWarn("token-expiry index: rebuild save failed", "error", err.Error())
		return
	}
	p.API.LogInfo("token-expiry index: rebuilt", "count", len(keys))
}

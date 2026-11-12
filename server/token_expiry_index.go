package main

import (
	"strings"

	"github.com/pkg/errors"
)

// kvTokenExpiryIndex holds bot_token_expiry_* keys so the daily reminder job
// does not scan the entire plugin KV store (histories, anchors, …).
const kvTokenExpiryIndex = "bot_token_expiry_index_v1" //nolint:gosec // G101: a KV key, not a credential

const tokenExpiryIndexCASAttempts = keyIndexCASAttempts

func (p *Plugin) tokenExpiryKeyIndex() keyIndex {
	return keyIndex{p: p, store: kvTokenExpiryIndex, label: "token-expiry"}
}

// listTokenExpiryKeys returns tracked token-expiry KV keys from the index,
// rebuilding from a full scan when the index is missing.
func (p *Plugin) listTokenExpiryKeys() ([]string, error) {
	return p.tokenExpiryKeyIndex().list(p.scanTokenExpiryKeys)
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
	return p.tokenExpiryKeyIndex().load()
}

func (p *Plugin) persistTokenExpiryIndexFromScan(prev []byte, scanned []string) error {
	return p.tokenExpiryKeyIndex().persistFromScan(prev, scanned)
}

func (p *Plugin) invalidateTokenExpiryIndex(failedKey string) {
	p.tokenExpiryKeyIndex().invalidate(failedKey)
}

func (p *Plugin) trackTokenExpiryKey(key string) {
	p.tokenExpiryKeyIndex().track(key)
}

func (p *Plugin) untrackTokenExpiryKey(key string) {
	p.tokenExpiryKeyIndex().untrack(key)
}

func (p *Plugin) rebuildTokenExpiryIndex() {
	p.tokenExpiryKeyIndex().rebuild(p.scanTokenExpiryKeys)
}

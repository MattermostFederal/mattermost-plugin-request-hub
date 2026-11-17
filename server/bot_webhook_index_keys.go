package main

import (
	"strings"

	"github.com/pkg/errors"
)

// Compact key indexes for bot_index_* and webhook_index_* so transfer pickers
// and inventory do not page the entire plugin KV store.

const (
	kvBotIndexKeys     = "bot_index_keys_v1"
	kvWebhookIndexKeys = "webhook_index_keys_v1"
)

func (p *Plugin) botIndexKeyIndex() keyIndex {
	return keyIndex{p: p, store: kvBotIndexKeys, label: "bot-index"}
}

func (p *Plugin) webhookIndexKeyIndex() keyIndex {
	return keyIndex{p: p, store: kvWebhookIndexKeys, label: "webhook-index"}
}

func (p *Plugin) listBotIndexKeys() ([]string, error) {
	return p.botIndexKeyIndex().list(p.scanBotIndexKeys)
}

func (p *Plugin) listWebhookIndexKeys() ([]string, error) {
	return p.webhookIndexKeyIndex().list(p.scanWebhookIndexKeys)
}

func (p *Plugin) scanBotIndexKeys() ([]string, error) {
	return p.scanPrefixedIndexKeys(kvBotIndexPrefix, kvBotIndexKeys)
}

func (p *Plugin) scanWebhookIndexKeys() ([]string, error) {
	return p.scanPrefixedIndexKeys(kvWebhookIndexPrefix, kvWebhookIndexKeys)
}

func (p *Plugin) scanPrefixedIndexKeys(prefix, indexStore string) ([]string, error) {
	const perPage = 200
	var keys []string
	for page := 0; ; page++ {
		batch, appErr := p.API.KVList(page, perPage)
		if appErr != nil {
			return nil, errors.Wrap(appErr, "failed to list index keys")
		}
		for _, key := range batch {
			if key == indexStore {
				continue
			}
			if strings.HasPrefix(key, prefix) {
				keys = append(keys, key)
			}
		}
		if len(batch) < perPage {
			return keys, nil
		}
	}
}

func (p *Plugin) trackBotIndexKey(botUserID string) {
	p.botIndexKeyIndex().track(kvBotIndexPrefix + botUserID)
}

func (p *Plugin) trackWebhookIndexKey(hookID string) {
	p.webhookIndexKeyIndex().track(webhookIndexKey(hookID))
}

func (p *Plugin) rebuildBotIndexKeys() {
	p.botIndexKeyIndex().rebuild(p.scanBotIndexKeys)
}

func (p *Plugin) rebuildWebhookIndexKeys() {
	p.webhookIndexKeyIndex().rebuild(p.scanWebhookIndexKeys)
}

package main

import (
	"encoding/json"
	"slices"

	"github.com/pkg/errors"
)

// kvOpenRequestIndex holds the KV keys of every currently pending request.
// listRequestKeys reads this one blob instead of scanning the whole plugin
// store (histories, anchors, bot index, …). Rebuilt from a full scan when
// missing or unreadable; store/claim paths keep it updated best-effort.
const kvOpenRequestIndex = "open_requests_v1"

const openRequestIndexCASAttempts = 8

// listRequestKeys returns the KV key of every stored pending request.
// Prefers the open-request index; rebuilds from a full KV scan when needed.
func (p *Plugin) listRequestKeys() ([]string, error) {
	if keys, ok := p.loadOpenRequestIndex(); ok {
		return keys, nil
	}
	keys, err := p.scanAllRequestKeys()
	if err != nil {
		return nil, err
	}
	if saveErr := p.saveOpenRequestIndex(keys); saveErr != nil {
		p.API.LogWarn("failed to persist open-request index; list still returned",
			"error", saveErr.Error(), "count", len(keys))
	}
	return keys, nil
}

// scanAllRequestKeys pages the entire plugin KV store for pending request
// keys. Expensive — used only to rebuild the index.
func (p *Plugin) scanAllRequestKeys() ([]string, error) {
	const perPage = 200
	var keys []string
	for page := 0; ; page++ {
		batch, appErr := p.API.KVList(page, perPage)
		if appErr != nil {
			return nil, errors.Wrap(appErr, "failed to list stored requests")
		}
		for _, key := range batch {
			if _, ok := pendingKindForKey(key); ok {
				keys = append(keys, key)
			}
		}
		if len(batch) < perPage {
			return keys, nil
		}
	}
}

func (p *Plugin) loadOpenRequestIndex() ([]string, bool) {
	raw, appErr := p.API.KVGet(kvOpenRequestIndex)
	if appErr != nil || raw == nil {
		return nil, false
	}
	var keys []string
	if err := json.Unmarshal(raw, &keys); err != nil {
		p.API.LogWarn("open-request index unreadable; will rebuild", "error", err.Error())
		return nil, false
	}
	return keys, true
}

func (p *Plugin) saveOpenRequestIndex(keys []string) error {
	if keys == nil {
		keys = []string{}
	}
	data, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	if appErr := p.API.KVSet(kvOpenRequestIndex, data); appErr != nil {
		return appErr
	}
	return nil
}

// trackOpenRequestKey adds kvKey to the open-request index after a successful
// store. Best-effort — a failed update only means the next list rebuilds.
func (p *Plugin) trackOpenRequestKey(kvKey string) {
	if kvKey == "" {
		return
	}
	for range openRequestIndexCASAttempts {
		raw, appErr := p.API.KVGet(kvOpenRequestIndex)
		if appErr != nil {
			p.API.LogWarn("open-request index: failed to load for add", "key", kvKey, "error", appErr.Error())
			return
		}
		var keys []string
		if raw != nil {
			if err := json.Unmarshal(raw, &keys); err != nil {
				p.rebuildOpenRequestIndex()
				continue
			}
		}
		if slices.Contains(keys, kvKey) {
			return
		}
		next := append(slices.Clone(keys), kvKey)
		data, err := json.Marshal(next)
		if err != nil {
			return
		}
		ok, casErr := p.API.KVCompareAndSet(kvOpenRequestIndex, raw, data)
		if casErr != nil {
			p.API.LogWarn("open-request index: failed to add key", "key", kvKey, "error", casErr.Error())
			return
		}
		if ok {
			return
		}
	}
	p.API.LogWarn("open-request index: gave up adding key after CAS contention", "key", kvKey)
}

// untrackOpenRequestKey removes kvKey from the open-request index after the
// request is claimed or deleted. Best-effort.
func (p *Plugin) untrackOpenRequestKey(kvKey string) {
	if kvKey == "" {
		return
	}
	for range openRequestIndexCASAttempts {
		raw, appErr := p.API.KVGet(kvOpenRequestIndex)
		if appErr != nil {
			p.API.LogWarn("open-request index: failed to load for remove", "key", kvKey, "error", appErr.Error())
			return
		}
		if raw == nil {
			return
		}
		var keys []string
		if err := json.Unmarshal(raw, &keys); err != nil {
			return
		}
		idx := slices.Index(keys, kvKey)
		if idx < 0 {
			return
		}
		next := slices.Delete(slices.Clone(keys), idx, idx+1)
		data, err := json.Marshal(next)
		if err != nil {
			return
		}
		ok, casErr := p.API.KVCompareAndSet(kvOpenRequestIndex, raw, data)
		if casErr != nil {
			p.API.LogWarn("open-request index: failed to remove key", "key", kvKey, "error", casErr.Error())
			return
		}
		if ok {
			return
		}
	}
	p.API.LogWarn("open-request index: gave up removing key after CAS contention", "key", kvKey)
}

// rebuildOpenRequestIndex rescans the store and replaces the index. Used on
// activate so upgrades pick up requests stored before indexing existed.
func (p *Plugin) rebuildOpenRequestIndex() {
	keys, err := p.scanAllRequestKeys()
	if err != nil {
		p.API.LogWarn("open-request index: rebuild scan failed", "error", err.Error())
		return
	}
	if err := p.saveOpenRequestIndex(keys); err != nil {
		p.API.LogWarn("open-request index: rebuild save failed", "error", err.Error())
		return
	}
	p.API.LogInfo("open-request index: rebuilt", "count", len(keys))
}

// putPendingRequestKV stores a pending request record and tracks it in the
// open-request index.
func (p *Plugin) putPendingRequestKV(key string, data []byte) error {
	if appErr := p.API.KVSet(key, data); appErr != nil {
		return appErr
	}
	p.trackOpenRequestKey(key)
	return nil
}

// deletePendingRequestKV deletes a pending request record and drops it from
// the open-request index. Used for submit rollbacks before a claim.
func (p *Plugin) deletePendingRequestKV(key string) {
	_ = p.API.KVDelete(key)
	p.untrackOpenRequestKey(key)
}

package main

import (
	"github.com/pkg/errors"
)

// kvOpenRequestIndex holds the KV keys of every currently pending request.
// listRequestKeys reads this one blob instead of scanning the whole plugin
// store (histories, anchors, bot index, …). Rebuilt from a full scan when
// missing or unreadable; store/claim paths keep it updated best-effort.
const kvOpenRequestIndex = "open_requests_v1"

// openRequestIndexCASAttempts kept as an alias so existing tests that loop
// on contention still compile against the shared CAS budget.
const openRequestIndexCASAttempts = keyIndexCASAttempts

func (p *Plugin) openRequestKeyIndex() keyIndex {
	return keyIndex{p: p, store: kvOpenRequestIndex, label: "open-request"}
}

// listRequestKeys returns the KV key of every stored pending request.
// Prefers the open-request index; rebuilds from a full KV scan when needed.
func (p *Plugin) listRequestKeys() ([]string, error) {
	return p.openRequestKeyIndex().list(p.scanAllRequestKeys)
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
	return p.openRequestKeyIndex().load()
}

func (p *Plugin) persistOpenRequestIndexFromScan(prev []byte, scanned []string) error {
	return p.openRequestKeyIndex().persistFromScan(prev, scanned)
}

func (p *Plugin) trackOpenRequestKey(kvKey string) {
	p.openRequestKeyIndex().track(kvKey)
}

func (p *Plugin) untrackOpenRequestKey(kvKey string) {
	p.openRequestKeyIndex().untrack(kvKey)
}

// rebuildOpenRequestIndex rescans the store and replaces the index. Used on
// activate and the periodic reconciler so rolling HA upgrades and older
// writers still get into the approval queue.
func (p *Plugin) rebuildOpenRequestIndex() {
	p.openRequestKeyIndex().rebuild(p.scanAllRequestKeys)
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
// the open-request index. Used for submit rollbacks before a claim. Untrack
// only after a successful delete so a failed KVDelete cannot hide a live
// request from the approval queue until the next index rebuild.
func (p *Plugin) deletePendingRequestKV(key string) {
	if appErr := p.API.KVDelete(key); appErr != nil {
		p.API.LogWarn("failed to delete pending request", "key", key, "error", appErr.Error())
		return
	}
	p.untrackOpenRequestKey(key)
}

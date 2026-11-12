package main

import (
	"bytes"
	"encoding/json"
	"slices"

	"github.com/pkg/errors"
)

const keyIndexCASAttempts = 8

// kvKeyIndexBlob is the object form of a key index. Complete indexes are
// still stored as a bare JSON string array for back-compat; this shape is
// used for partial indexes written while a rebuild is in flight so a
// concurrent track cannot be lost when the rebuild publishes.
type kvKeyIndexBlob struct {
	Complete bool     `json:"complete"`
	Keys     []string `json:"keys"`
}

// parseKeyIndex decodes a complete []string blob or a kvKeyIndexBlob object.
// ok is false when raw is unreadable.
func parseKeyIndex(raw []byte) (keys []string, complete bool, ok bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, false, false
	}
	if raw[0] == '[' {
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, false, false
		}
		return list, true, true
	}
	var blob kvKeyIndexBlob
	if err := json.Unmarshal(raw, &blob); err != nil {
		return nil, false, false
	}
	return blob.Keys, blob.Complete, true
}

func marshalPartialKeyIndex(keys []string) ([]byte, error) {
	if keys == nil {
		keys = []string{}
	}
	return json.Marshal(kvKeyIndexBlob{Complete: false, Keys: keys})
}

func marshalCompleteKeyIndex(keys []string) ([]byte, error) {
	if keys == nil {
		keys = []string{}
	}
	// Bare array stays the on-disk complete form so upgrades keep reading
	// existing indexes and tests that assert []string still apply.
	return json.Marshal(keys)
}

func unionStringKeys(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, keys := range [][]string{a, b} {
		for _, k := range keys {
			if k == "" {
				continue
			}
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, k)
		}
	}
	return out
}

// keyIndex is the shared CAS index used by open-request and token-expiry
// tracking: list/load/persist/invalidate/track/untrack/rebuild.
type keyIndex struct {
	p     *Plugin
	store string // KV key for the index blob
	label string // log label, e.g. "open-request"
}

func (idx keyIndex) load() ([]string, bool) {
	raw, appErr := idx.p.API.KVGet(idx.store)
	if appErr != nil || raw == nil {
		return nil, false
	}
	keys, complete, ok := parseKeyIndex(raw)
	if !ok {
		idx.p.API.LogWarn(idx.label+" index unreadable; will rebuild")
		return nil, false
	}
	// Partial blobs are not readable as a complete index — rebuild so we
	// never serve a one-key (or otherwise incomplete) view as authoritative.
	if !complete {
		return nil, false
	}
	return keys, true
}

func (idx keyIndex) list(scan func() ([]string, error)) ([]string, error) {
	if keys, ok := idx.load(); ok {
		return keys, nil
	}
	// Snapshot before the slow scan so a concurrent track/untrack is not
	// clobbered by an unconditional KVSet of a stale scan result.
	prev, _ := idx.p.API.KVGet(idx.store)
	keys, err := scan()
	if err != nil {
		return nil, err
	}
	if saveErr := idx.persistFromScan(prev, keys); saveErr != nil {
		idx.p.API.LogWarn("failed to persist "+idx.label+" index; list still returned",
			"error", saveErr.Error(), "count", len(keys))
	}
	return keys, nil
}

// persistFromScan writes a scan result with CAS against prev (the index
// value observed before the scan). On conflict it unions the scan with the
// current index (complete or partial) and retries, so concurrent
// track/untrack wins are not wiped by a slower rebuild.
func (idx keyIndex) persistFromScan(prev []byte, scanned []string) error {
	keys := scanned
	if keys == nil {
		keys = []string{}
	}
	expected := prev
	for range keyIndexCASAttempts {
		data, err := marshalCompleteKeyIndex(keys)
		if err != nil {
			return err
		}
		ok, casErr := idx.p.API.KVCompareAndSet(idx.store, expected, data)
		if casErr != nil {
			return casErr
		}
		if ok {
			return nil
		}
		raw, appErr := idx.p.API.KVGet(idx.store)
		if appErr != nil {
			return appErr
		}
		var current []string
		if raw != nil {
			parsed, _, parseOK := parseKeyIndex(raw)
			if !parseOK {
				// Unreadable competing value — keep trying to replace it.
				expected = raw
				continue
			}
			current = parsed
		}
		keys = unionStringKeys(scanned, current)
		expected = raw
	}
	return errors.New(idx.label + " index: CAS contention persisting scan")
}

// invalidate marks the index incomplete by writing a partial blob that
// includes failedKey (when set). Prefer this over KVDelete: a rebuild in
// flight that loses its CAS then reads nil would publish a scan that missed
// the failed key; a partial forces that rebuild to union the key in.
func (idx keyIndex) invalidate(failedKey string) {
	keys := []string{}
	if failedKey != "" {
		keys = []string{failedKey}
	}
	data, err := marshalPartialKeyIndex(keys)
	if err != nil {
		idx.p.API.LogWarn(idx.label+" index: failed to marshal invalidate blob", "error", err.Error())
		return
	}
	for range keyIndexCASAttempts {
		raw, appErr := idx.p.API.KVGet(idx.store)
		if appErr != nil {
			idx.p.API.LogWarn(idx.label+" index: failed to invalidate", "error", appErr.Error())
			return
		}
		if raw != nil {
			if existing, _, ok := parseKeyIndex(raw); ok {
				data, err = marshalPartialKeyIndex(unionStringKeys(existing, keys))
				if err != nil {
					return
				}
			}
		}
		ok, casErr := idx.p.API.KVCompareAndSet(idx.store, raw, data)
		if casErr != nil {
			idx.p.API.LogWarn(idx.label+" index: failed to invalidate", "error", casErr.Error())
			return
		}
		if ok {
			return
		}
	}
	idx.p.API.LogWarn(idx.label + " index: invalidate exhausted CAS retries")
}

// track adds kvKey after a successful store. Best-effort — a failed update
// invalidates with that key so the next list rebuilds rather than trusting a
// partial blob. When the complete index is missing, writes a partial blob
// (complete:false) so a concurrent rebuild unions this key instead of
// publishing a scan that missed it.
func (idx keyIndex) track(kvKey string) {
	if kvKey == "" {
		return
	}
	for range keyIndexCASAttempts {
		raw, appErr := idx.p.API.KVGet(idx.store)
		if appErr != nil {
			idx.p.API.LogWarn(idx.label+" index: failed to load for add", "key", kvKey, "error", appErr.Error())
			idx.invalidate(kvKey)
			return
		}
		if raw == nil {
			data, err := marshalPartialKeyIndex([]string{kvKey})
			if err != nil {
				idx.invalidate(kvKey)
				return
			}
			ok, casErr := idx.p.API.KVCompareAndSet(idx.store, nil, data)
			if casErr != nil {
				idx.p.API.LogWarn(idx.label+" index: failed to add key", "key", kvKey, "error", casErr.Error())
				idx.invalidate(kvKey)
				return
			}
			if ok {
				return
			}
			continue
		}
		keys, complete, ok := parseKeyIndex(raw)
		if !ok {
			idx.invalidate(kvKey)
			return
		}
		if slices.Contains(keys, kvKey) {
			return
		}
		next := append(slices.Clone(keys), kvKey)
		var data []byte
		var err error
		if complete {
			data, err = marshalCompleteKeyIndex(next)
		} else {
			data, err = marshalPartialKeyIndex(next)
		}
		if err != nil {
			idx.invalidate(kvKey)
			return
		}
		casOK, casErr := idx.p.API.KVCompareAndSet(idx.store, raw, data)
		if casErr != nil {
			idx.p.API.LogWarn(idx.label+" index: failed to add key", "key", kvKey, "error", casErr.Error())
			idx.invalidate(kvKey)
			return
		}
		if casOK {
			return
		}
	}
	idx.p.API.LogWarn(idx.label+" index: gave up adding key after CAS contention", "key", kvKey)
	idx.invalidate(kvKey)
}

// untrack removes kvKey after the record is claimed or deleted. Best-effort.
func (idx keyIndex) untrack(kvKey string) {
	if kvKey == "" {
		return
	}
	for range keyIndexCASAttempts {
		raw, appErr := idx.p.API.KVGet(idx.store)
		if appErr != nil {
			idx.p.API.LogWarn(idx.label+" index: failed to load for remove", "key", kvKey, "error", appErr.Error())
			return
		}
		if raw == nil {
			return
		}
		keys, complete, ok := parseKeyIndex(raw)
		if !ok {
			return
		}
		i := slices.Index(keys, kvKey)
		if i < 0 {
			return
		}
		next := slices.Delete(slices.Clone(keys), i, i+1)
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
		casOK, casErr := idx.p.API.KVCompareAndSet(idx.store, raw, data)
		if casErr != nil {
			idx.p.API.LogWarn(idx.label+" index: failed to remove key", "key", kvKey, "error", casErr.Error())
			return
		}
		if casOK {
			return
		}
	}
	idx.p.API.LogWarn(idx.label+" index: gave up removing key after CAS contention", "key", kvKey)
}

func (idx keyIndex) rebuild(scan func() ([]string, error)) {
	prev, _ := idx.p.API.KVGet(idx.store)
	keys, err := scan()
	if err != nil {
		idx.p.API.LogWarn(idx.label+" index: rebuild scan failed", "error", err.Error())
		return
	}
	if err := idx.persistFromScan(prev, keys); err != nil {
		idx.p.API.LogWarn(idx.label+" index: rebuild save failed", "error", err.Error())
		return
	}
	idx.p.API.LogInfo(idx.label+" index: rebuilt", "count", len(keys))
}

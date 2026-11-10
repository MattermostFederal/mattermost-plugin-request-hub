package main

import (
	"bytes"
	"encoding/json"
)

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

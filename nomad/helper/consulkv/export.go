// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consulkv

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// exportVersion is the version of the export format.
const exportVersion = 1

// Export is the content of the key-value bucket of the Consul KV in a
// backup: every key with its value.
type Export struct {
	Version int
	Bucket  string
	Entries []*Entry
}

// Entry is a key of the Consul KV: the Consul key (not the NATS key), its
// value and the revision it had in the bucket.
type Entry struct {
	Key      string
	Value    []byte
	Revision uint64
	Created  time.Time
}

// Encode returns the JSON of an export.
func Encode(e *Export) ([]byte, error) {
	e.Version = exportVersion
	return json.Marshal(e)
}

// Decode reads an export.
func Decode(r io.Reader) (*Export, error) {
	var e Export
	if err := json.NewDecoder(r).Decode(&e); err != nil {
		return nil, fmt.Errorf("invalid Consul KV export: %w", err)
	}
	if e.Version != exportVersion {
		return nil, fmt.Errorf("unsupported Consul KV export version %d", e.Version)
	}
	return &e, nil
}

// Entry returns the entry of a key, nil if the export has none.
func (e *Export) Entry(key string) *Entry {
	if e == nil {
		return nil
	}
	for _, en := range e.Entries {
		if en.Key == key {
			return en
		}
	}
	return nil
}

// Package snapshot serializes and restores the full contents of a storage
// backend (docs/DESIGN.md §8.3). A snapshot is a consistent copy of every stored
// entry — values are the barrier's ciphertext, so the snapshot never contains
// plaintext and a restored copy still requires the unseal shares (or the
// auto-unseal KEK) to be usable.
//
// Format: a header line ("ubixvault-snapshot v1"), then one JSON object per
// entry, newline-delimited: {"key": "...", "value": "<base64>"}.
package snapshot

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// header identifies the snapshot format and version.
const header = "ubixvault-snapshot v1"

type entry struct {
	Key   string `json:"key"`
	Value []byte `json:"value"` // encrypted at rest; JSON-encoded as base64
}

// Write serializes every entry in b to w.
func Write(ctx context.Context, b storage.Backend, w io.Writer) error {
	bw := bufio.NewWriter(w)
	if _, err := fmt.Fprintln(bw, header); err != nil {
		return fmt.Errorf("snapshot: write header: %w", err)
	}
	enc := json.NewEncoder(bw)
	err := storage.Walk(ctx, b, func(e *storage.Entry) error {
		return enc.Encode(entry{Key: e.Key, Value: e.Value})
	})
	if err != nil {
		return fmt.Errorf("snapshot: write entries: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("snapshot: flush: %w", err)
	}
	return nil
}

// Restore reads a snapshot from r and writes every entry into b. The caller is
// responsible for restoring into a fresh/appropriate backend; existing keys with
// the same names are overwritten.
func Restore(ctx context.Context, b storage.Backend, r io.Reader) error {
	br := bufio.NewReader(r)
	line, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("snapshot: read header: %w", err)
	}
	if got := line[:len(line)-1]; got != header {
		return fmt.Errorf("snapshot: unrecognized format %q", got)
	}

	dec := json.NewDecoder(br)
	for {
		var e entry
		if err := dec.Decode(&e); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("snapshot: decode entry: %w", err)
		}
		if err := b.Put(ctx, &storage.Entry{Key: e.Key, Value: e.Value}); err != nil {
			return fmt.Errorf("snapshot: restore %q: %w", e.Key, err)
		}
	}
}

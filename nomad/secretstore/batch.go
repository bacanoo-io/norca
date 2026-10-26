// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secretstore

import (
	"context"
	"sort"
	"sync"

	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/secretstore/ubix/export/storage"
)

// Batch collects the writes the barrier makes for one operation of the
// secret store, instead of applying each of them through Raft. The store
// then applies them together with the secret's metadata row in a single
// Raft entry, so the encrypted values and the metadata can never diverge.
//
// A context carrying a batch (see [WithBatch]) changes the Backend's
// behaviour for calls made with it:
//
//   - Put and Delete record the change in the batch and return; nothing is
//     written. The key and size checks still apply.
//   - Get reads the batch first, so an operation reads its own writes;
//     entries not in the batch are read from the state store.
//
// List does not see the batch. The KV engine never lists within an
// operation.
type Batch struct {
	mu sync.Mutex
	// puts holds the values written, deletes the keys deleted; a key is in
	// at most one of them.
	puts    map[string][]byte
	deletes map[string]struct{}
}

type batchKey struct{}

// WithBatch returns a context whose Backend writes go to a new batch, and
// the batch.
func WithBatch(ctx context.Context) (context.Context, *Batch) {
	b := &Batch{puts: map[string][]byte{}, deletes: map[string]struct{}{}}
	return context.WithValue(ctx, batchKey{}, b), b
}

// batchFrom returns the batch of the context, or nil.
func batchFrom(ctx context.Context) *Batch {
	b, _ := ctx.Value(batchKey{}).(*Batch)
	return b
}

// Preload adds entries to the batch as if they had been written, so that
// reads with the batch's context see them. It is used to read entries that
// are not in the store (for example from a backup) through the barrier.
func (b *Batch) Preload(blobs []*structs.SecretStoreBlob) {
	for _, blob := range blobs {
		b.put(blob.Path, blob.Value)
	}
}

func (b *Batch) put(key string, value []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.puts[key] = append([]byte(nil), value...)
	delete(b.deletes, key)
}

func (b *Batch) delete(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.puts, key)
	b.deletes[key] = struct{}{}
}

// get returns the batch's view of key: the value written, or deleted true if
// it was deleted; ok is false if the batch has no change for the key.
func (b *Batch) get(key string) (value []byte, deleted, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if v, found := b.puts[key]; found {
		return append([]byte(nil), v...), false, true
	}
	if _, found := b.deletes[key]; found {
		return nil, true, true
	}
	return nil, false, false
}

// Blobs returns the values written, ordered by path.
func (b *Batch) Blobs() []*structs.SecretStoreBlob {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*structs.SecretStoreBlob, 0, len(b.puts))
	for k, v := range b.puts {
		out = append(out, &structs.SecretStoreBlob{Path: k, Value: append([]byte(nil), v...)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Deletes returns the keys deleted, sorted.
func (b *Batch) Deletes() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.deletes))
	for k := range b.deletes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Empty reports whether the batch holds no change.
func (b *Batch) Empty() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.puts) == 0 && len(b.deletes) == 0
}

// getBatched serves Get from the batch of the context, if it has a change
// for the key.
func getBatched(ctx context.Context, key string) (*storage.Entry, bool) {
	b := batchFrom(ctx)
	if b == nil {
		return nil, false
	}
	v, deleted, ok := b.get(key)
	if !ok {
		return nil, false
	}
	if deleted {
		return nil, true
	}
	return &storage.Entry{Key: key, Value: v}, true
}

// CheckWrite reports whether this backend may write now: every server of
// the region supports the secret store, and, once a core uses the HA lock,
// this server holds it. The store calls it before it applies a batch.
func (b *Backend) CheckWrite(ctx context.Context) error {
	return b.checkWrite(ctx)
}

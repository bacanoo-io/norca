package storage

import (
	"context"
	"sort"
	"sync"
)

// MemoryBackend is an in-memory [Backend]. It is intended for tests and
// ephemeral use; nothing is persisted across process restarts.
type MemoryBackend struct {
	mu   sync.RWMutex
	data map[string][]byte
}

// NewMemoryBackend returns an empty in-memory backend.
func NewMemoryBackend() *MemoryBackend {
	return &MemoryBackend{data: make(map[string][]byte)}
}

var _ Backend = (*MemoryBackend)(nil)

// Get implements [Backend].
func (b *MemoryBackend) Get(_ context.Context, key string) (*Entry, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()

	v, ok := b.data[key]
	if !ok {
		return nil, nil
	}
	// Return a copy so callers cannot mutate stored bytes through the returned slice.
	return &Entry{Key: key, Value: append([]byte(nil), v...)}, nil
}

// Put implements [Backend].
func (b *MemoryBackend) Put(_ context.Context, entry *Entry) error {
	if err := ValidateKey(entry.Key); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	// Copy so later mutation of the caller's slice does not affect stored bytes.
	b.data[entry.Key] = append([]byte(nil), entry.Value...)
	return nil
}

// Delete implements [Backend].
func (b *MemoryBackend) Delete(_ context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	delete(b.data, key)
	return nil
}

// List implements [Backend].
func (b *MemoryBackend) List(_ context.Context, prefix string) ([]string, error) {
	if err := validatePrefix(prefix); err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()

	seen := make(map[string]struct{})
	for key := range b.data {
		if child, ok := childUnder(prefix, key); ok {
			seen[child] = struct{}{}
		}
	}

	out := make([]string, 0, len(seen))
	for child := range seen {
		out = append(out, child)
	}
	sort.Strings(out)
	return out, nil
}

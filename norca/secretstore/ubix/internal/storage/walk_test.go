package storage

import (
	"context"
	"sort"
	"testing"
)

func TestWalkVisitsAllLeaves(t *testing.T) {
	ctx := context.Background()
	b := NewMemoryBackend()
	keys := []string{"a", "b/c", "b/d", "e/f/g", "e/f/h", "top"}
	for _, k := range keys {
		if err := b.Put(ctx, &Entry{Key: k, Value: []byte(k)}); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}

	var got []string
	if err := Walk(ctx, b, func(e *Entry) error {
		got = append(got, e.Key)
		if string(e.Value) != e.Key {
			t.Errorf("entry %q has value %q", e.Key, e.Value)
		}
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	sort.Strings(got)
	sort.Strings(keys)
	if len(got) != len(keys) {
		t.Fatalf("walked %v, want %v", got, keys)
	}
	for i := range keys {
		if got[i] != keys[i] {
			t.Fatalf("walked %v, want %v", got, keys)
		}
	}
}

func TestWalkEmpty(t *testing.T) {
	count := 0
	err := Walk(context.Background(), NewMemoryBackend(), func(*Entry) error { count++; return nil })
	if err != nil || count != 0 {
		t.Fatalf("walk empty: count=%d err=%v", count, err)
	}
}

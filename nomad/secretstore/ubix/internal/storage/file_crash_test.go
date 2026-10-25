package storage

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// TestFileBackendIgnoresLeftoverTempFiles simulates a crash mid-write: the atomic
// Put leaves a temp file behind if the process dies before the rename. Such a
// leftover must never surface as a data key in List, Walk, or a snapshot, and the
// real data must be intact.
func TestFileBackendIgnoresLeftoverTempFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	b, err := NewFileBackend(dir)
	if err != nil {
		t.Fatalf("NewFileBackend: %v", err)
	}

	if err := b.Put(ctx, &Entry{Key: "app/db", Value: []byte("cipher")}); err != nil {
		t.Fatalf("Put app/db: %v", err)
	}
	if err := b.Put(ctx, &Entry{Key: "top", Value: []byte("x")}); err != nil {
		t.Fatalf("Put top: %v", err)
	}

	// Drop leftover temp files (as a crashed Put would) at the root and in app/.
	for _, p := range []string{
		filepath.Join(dir, tempFilePrefix+"aaaa"),
		filepath.Join(dir, "app", tempFilePrefix+"bbbb"),
	} {
		if err := os.WriteFile(p, []byte("half-written garbage"), 0o600); err != nil {
			t.Fatalf("seed temp file %q: %v", p, err)
		}
	}

	// List must not expose the temp files as keys.
	if got, _ := b.List(ctx, ""); !reflect.DeepEqual(got, []string{"app/", "top"}) {
		t.Fatalf(`List("") = %v, want [app/ top]`, got)
	}
	if got, _ := b.List(ctx, "app/"); !reflect.DeepEqual(got, []string{"db"}) {
		t.Fatalf(`List("app/") = %v, want [db]`, got)
	}

	// Walk (used by snapshots) must yield only the real entries.
	var keys []string
	if err := Walk(ctx, b, func(e *Entry) error { keys = append(keys, e.Key); return nil }); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"app/db", "top"}) {
		t.Fatalf("Walk keys = %v, want [app/db top]", keys)
	}

	// The real data is untouched.
	if e, _ := b.Get(ctx, "app/db"); e == nil || string(e.Value) != "cipher" {
		t.Fatalf("Get app/db = %+v, want cipher", e)
	}
}

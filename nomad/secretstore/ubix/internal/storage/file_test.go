package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFileBackend(t *testing.T) {
	runBackendConformance(t, func(t *testing.T) Backend {
		b, err := NewFileBackend(t.TempDir())
		if err != nil {
			t.Fatalf("NewFileBackend: %v", err)
		}
		return b
	})
}

func TestNewFileBackendRejectsEmptyRoot(t *testing.T) {
	if _, err := NewFileBackend(""); err == nil {
		t.Fatal("NewFileBackend(\"\"): want error, got nil")
	}
}

// TestFileBackendPersistsAcrossReopen verifies data survives a fresh backend
// pointed at the same directory — the property an in-memory backend lacks.
func TestFileBackendPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	b1, err := NewFileBackend(dir)
	if err != nil {
		t.Fatalf("NewFileBackend: %v", err)
	}
	if err := b1.Put(ctx, &Entry{Key: "a/b", Value: []byte("durable")}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	b2, err := NewFileBackend(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := b2.Get(ctx, "a/b")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || string(got.Value) != "durable" {
		t.Fatalf("reopened value = %+v, want durable", got)
	}
}

// TestFileBackendPermissions checks that on-disk files and directories are not
// group- or world-accessible — defense in depth for stored (encrypted) blobs.
func TestFileBackendPermissions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	b, err := NewFileBackend(dir)
	if err != nil {
		t.Fatalf("NewFileBackend: %v", err)
	}
	if err := b.Put(ctx, &Entry{Key: "secrets/api-key", Value: []byte("v")}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	fi, err := os.Stat(filepath.Join(dir, "secrets", filePrefix+"api-key"))
	if err != nil {
		t.Fatalf("stat leaf: %v", err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("leaf file perm = %o, want no group/world bits", perm)
	}

	di, err := os.Stat(filepath.Join(dir, "secrets"))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("dir perm = %o, want no group/world bits", perm)
	}
}

// TestFileBackendPrunesEmptyDirs verifies Delete cleans up now-empty parent
// directories, without ever removing the root.
func TestFileBackendPrunesEmptyDirs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	b, err := NewFileBackend(dir)
	if err != nil {
		t.Fatalf("NewFileBackend: %v", err)
	}
	if err := b.Put(ctx, &Entry{Key: "a/b/c", Value: []byte("v")}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := b.Delete(ctx, "a/b/c"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); !os.IsNotExist(err) {
		t.Errorf("empty dir 'a' not pruned (err=%v)", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("root must survive pruning: %v", err)
	}
}

// TestFileBackendLeafAndSubtreeCoexist is the collision case the "_" filename
// prefix exists to solve: a leaf "a/b" and a subtree "a/b/c" at the same path.
func TestFileBackendLeafAndSubtreeCoexist(t *testing.T) {
	ctx := context.Background()
	b, err := NewFileBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileBackend: %v", err)
	}
	if err := b.Put(ctx, &Entry{Key: "a/b", Value: []byte("leaf")}); err != nil {
		t.Fatalf("Put leaf: %v", err)
	}
	if err := b.Put(ctx, &Entry{Key: "a/b/c", Value: []byte("under")}); err != nil {
		t.Fatalf("Put subtree: %v", err)
	}

	leaf, err := b.Get(ctx, "a/b")
	if err != nil || leaf == nil || string(leaf.Value) != "leaf" {
		t.Fatalf("leaf Get = %+v, err=%v", leaf, err)
	}
	under, err := b.Get(ctx, "a/b/c")
	if err != nil || under == nil || string(under.Value) != "under" {
		t.Fatalf("subtree Get = %+v, err=%v", under, err)
	}

	children, err := b.List(ctx, "a/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var hasLeaf, hasDir bool
	for _, c := range children {
		switch c {
		case "b":
			hasLeaf = true
		case "b/":
			hasDir = true
		}
	}
	if !hasLeaf || !hasDir {
		t.Errorf("List(a/) = %v, want both 'b' and 'b/'", children)
	}
}

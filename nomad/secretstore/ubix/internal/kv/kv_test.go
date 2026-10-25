package kv

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"reflect"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/barrier"
	"github.com/cwolsen7905/ubixvault/internal/storage"
)

func newEngine() *Engine {
	return New(storage.NewMemoryBackend(), "secret")
}

func secret(v string) map[string]any { return map[string]any{"value": v} }

func TestWriteReadLatest(t *testing.T) {
	ctx := context.Background()
	e := newEngine()

	vm, err := e.Write(ctx, "app/db", secret("s3cr3t"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if vm.Version != 1 {
		t.Fatalf("first version = %d, want 1", vm.Version)
	}
	data, meta, err := e.Read(ctx, "app/db", 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !reflect.DeepEqual(data, secret("s3cr3t")) {
		t.Fatalf("data = %v", data)
	}
	if meta.Version != 1 {
		t.Fatalf("read version = %d", meta.Version)
	}
}

func TestVersioning(t *testing.T) {
	ctx := context.Background()
	e := newEngine()

	for i, v := range []string{"v1", "v2", "v3"} {
		vm, err := e.Write(ctx, "p", secret(v))
		if err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
		if vm.Version != i+1 {
			t.Fatalf("version = %d, want %d", vm.Version, i+1)
		}
	}

	// Latest is v3.
	data, _, _ := e.Read(ctx, "p", 0)
	if data["value"] != "v3" {
		t.Fatalf("latest = %v, want v3", data["value"])
	}
	// Specific older versions.
	for ver, want := range map[int]string{1: "v1", 2: "v2", 3: "v3"} {
		data, _, err := e.Read(ctx, "p", ver)
		if err != nil {
			t.Fatalf("Read v%d: %v", ver, err)
		}
		if data["value"] != want {
			t.Fatalf("v%d = %v, want %s", ver, data["value"], want)
		}
	}
}

func TestReadMissing(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if _, _, err := e.Read(ctx, "nope", 0); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("want ErrSecretNotFound, got %v", err)
	}
	_, _ = e.Write(ctx, "p", secret("x"))
	if _, _, err := e.Read(ctx, "p", 9); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("want ErrVersionNotFound, got %v", err)
	}
}

func TestSoftDeleteAndUndelete(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	_, _ = e.Write(ctx, "p", secret("x"))

	if err := e.Delete(ctx, "p"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := e.Read(ctx, "p", 0); !errors.Is(err, ErrVersionDeleted) {
		t.Fatalf("read after delete: want ErrVersionDeleted, got %v", err)
	}
	if err := e.Undelete(ctx, "p", 1); err != nil {
		t.Fatalf("Undelete: %v", err)
	}
	data, _, err := e.Read(ctx, "p", 0)
	if err != nil || data["value"] != "x" {
		t.Fatalf("read after undelete: data=%v err=%v", data, err)
	}
}

func TestDestroy(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	_, _ = e.Write(ctx, "p", secret("v1"))
	_, _ = e.Write(ctx, "p", secret("v2"))

	if err := e.Destroy(ctx, "p", 1); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, _, err := e.Read(ctx, "p", 1); !errors.Is(err, ErrVersionDestroyed) {
		t.Fatalf("read destroyed: want ErrVersionDestroyed, got %v", err)
	}
	// The data blob must be gone from storage.
	raw, _ := e.store.Get(ctx, e.dataKey("p", 1))
	if raw != nil {
		t.Fatal("destroyed version data still present in storage")
	}
	// A live version is unaffected.
	if data, _, err := e.Read(ctx, "p", 2); err != nil || data["value"] != "v2" {
		t.Fatalf("v2 after destroy v1: data=%v err=%v", data, err)
	}
}

func TestMaxVersionsAgesOutOldest(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	e.maxVer = 3

	for i := 0; i < 5; i++ {
		if _, err := e.Write(ctx, "p", secret("v")); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	meta, err := e.ReadMetadata(ctx, "p")
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if meta.CurrentVersion != 5 || meta.OldestVersion != 3 {
		t.Fatalf("after 5 writes max 3: current=%d oldest=%d", meta.CurrentVersion, meta.OldestVersion)
	}
	// Aged-out version 1: data gone, not readable.
	if raw, _ := e.store.Get(ctx, e.dataKey("p", 1)); raw != nil {
		t.Fatal("aged-out version 1 data still present")
	}
	if _, _, err := e.Read(ctx, "p", 1); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("read aged-out version: want ErrVersionNotFound, got %v", err)
	}
}

func TestListSecrets(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	_, _ = e.Write(ctx, "a", secret("x"))
	_, _ = e.Write(ctx, "b/c", secret("x"))
	_, _ = e.Write(ctx, "b/d", secret("x"))

	root, err := e.List(ctx, "")
	if err != nil {
		t.Fatalf("List root: %v", err)
	}
	if !reflect.DeepEqual(root, []string{"a", "b/"}) {
		t.Fatalf("List root = %v, want [a b/]", root)
	}
	under, _ := e.List(ctx, "b")
	if !reflect.DeepEqual(under, []string{"c", "d"}) {
		t.Fatalf("List b = %v, want [c d]", under)
	}
}

func TestDeleteMetadataRemovesEverything(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	_, _ = e.Write(ctx, "p", secret("v1"))
	_, _ = e.Write(ctx, "p", secret("v2"))

	if err := e.DeleteMetadata(ctx, "p"); err != nil {
		t.Fatalf("DeleteMetadata: %v", err)
	}
	if _, err := e.ReadMetadata(ctx, "p"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("metadata after delete: want ErrSecretNotFound, got %v", err)
	}
	for v := 1; v <= 2; v++ {
		if raw, _ := e.store.Get(ctx, e.dataKey("p", v)); raw != nil {
			t.Fatalf("version %d data survived DeleteMetadata", v)
		}
	}
}

func TestReadMetadataContent(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	_, _ = e.Write(ctx, "p", secret("v1"))
	_, _ = e.Write(ctx, "p", secret("v2"))

	meta, err := e.ReadMetadata(ctx, "p")
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if meta.CurrentVersion != 2 || meta.OldestVersion != 1 || len(meta.Versions) != 2 {
		t.Fatalf("metadata = %+v", meta)
	}
	if meta.MaxVersions != DefaultMaxVersions {
		t.Fatalf("max_versions = %d, want %d", meta.MaxVersions, DefaultMaxVersions)
	}
}

func TestDeleteOnMissingSecret(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if err := e.Delete(ctx, "nope"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("Delete missing: want ErrSecretNotFound, got %v", err)
	}
}

func TestDeleteMetadataOnMissingSecretIsNoOp(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if err := e.DeleteMetadata(ctx, "nope"); err != nil {
		t.Fatalf("DeleteMetadata missing: want nil, got %v", err)
	}
}

func TestDeleteSpecificVersion(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	_, _ = e.Write(ctx, "p", secret("v1"))
	_, _ = e.Write(ctx, "p", secret("v2"))

	if err := e.Delete(ctx, "p", 1); err != nil {
		t.Fatalf("Delete v1: %v", err)
	}
	if _, _, err := e.Read(ctx, "p", 1); !errors.Is(err, ErrVersionDeleted) {
		t.Fatalf("read deleted v1: want ErrVersionDeleted, got %v", err)
	}
	if data, _, err := e.Read(ctx, "p", 2); err != nil || data["value"] != "v2" {
		t.Fatalf("v2 should be intact: data=%v err=%v", data, err)
	}
}

func TestInvalidPath(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	if _, err := e.Write(ctx, "", secret("x")); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("empty path: want ErrInvalidPath, got %v", err)
	}
}

// TestEncryptedAtRestThroughBarrier mounts the engine on an unsealed barrier and
// confirms secret material never appears in the underlying store.
func TestEncryptedAtRestThroughBarrier(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryBackend()
	b := barrier.New(mem)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if err := b.Initialize(ctx, key); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := b.Unseal(ctx, key); err != nil {
		t.Fatalf("Unseal: %v", err)
	}

	e := New(b, "secret")
	if _, err := e.Write(ctx, "apikey", map[string]any{"token": "supersecretvalue"}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Read back through the barrier: plaintext round-trips.
	data, _, err := e.Read(ctx, "apikey", 0)
	if err != nil || data["token"] != "supersecretvalue" {
		t.Fatalf("round-trip through barrier: data=%v err=%v", data, err)
	}
	// But the raw stored blob must not contain the plaintext.
	raw, _ := mem.Get(ctx, "secret/data/apikey/1")
	if raw == nil {
		t.Fatal("expected an encrypted blob at the data key")
	}
	if bytes.Contains(raw.Value, []byte("supersecretvalue")) {
		t.Fatal("plaintext secret found in storage — not encrypted at rest")
	}
}

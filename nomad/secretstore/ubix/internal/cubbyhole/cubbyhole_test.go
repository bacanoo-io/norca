package cubbyhole

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

func newEngine() *Engine {
	return New(storage.NewMemoryBackend(), "cubbyhole")
}

func secret(v string) map[string]any { return map[string]any{"value": v} }

func TestWriteReadRoundTrip(t *testing.T) {
	ctx := context.Background()
	e := newEngine()

	if err := e.Write(ctx, "uv.tokenA", "creds", secret("s3cr3t")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := e.Read(ctx, "uv.tokenA", "creds")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !reflect.DeepEqual(got, secret("s3cr3t")) {
		t.Fatalf("Read = %v, want %v", got, secret("s3cr3t"))
	}
}

// TestScopedToToken is the core guarantee: a path written by one token is
// invisible to another, even at the same path.
func TestScopedToToken(t *testing.T) {
	ctx := context.Background()
	e := newEngine()

	if err := e.Write(ctx, "uv.tokenA", "note", secret("for-A")); err != nil {
		t.Fatalf("Write A: %v", err)
	}
	if err := e.Write(ctx, "uv.tokenB", "note", secret("for-B")); err != nil {
		t.Fatalf("Write B: %v", err)
	}

	a, err := e.Read(ctx, "uv.tokenA", "note")
	if err != nil {
		t.Fatalf("Read A: %v", err)
	}
	if a["value"] != "for-A" {
		t.Fatalf("token A read the wrong value: %v", a)
	}
	// A token with nothing written sees nothing at that path.
	if _, err := e.Read(ctx, "uv.tokenC", "note"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("token C read = %v, want ErrSecretNotFound", err)
	}
}

func TestReadMissing(t *testing.T) {
	if _, err := newEngine().Read(context.Background(), "uv.t", "nope"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("Read missing = %v, want ErrSecretNotFound", err)
	}
}

func TestDeleteRemoves(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	_ = e.Write(ctx, "uv.t", "k", secret("v"))
	if err := e.Delete(ctx, "uv.t", "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := e.Read(ctx, "uv.t", "k"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("after Delete, Read = %v, want ErrSecretNotFound", err)
	}
	// Deleting an absent path is a no-op.
	if err := e.Delete(ctx, "uv.t", "gone"); err != nil {
		t.Fatalf("Delete absent: %v", err)
	}
}

func TestList(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	_ = e.Write(ctx, "uv.t", "a", secret("1"))
	_ = e.Write(ctx, "uv.t", "b", secret("2"))
	_ = e.Write(ctx, "uv.t", "sub/c", secret("3"))

	root, err := e.List(ctx, "uv.t", "")
	if err != nil {
		t.Fatalf("List root: %v", err)
	}
	sort.Strings(root)
	if !reflect.DeepEqual(root, []string{"a", "b", "sub/"}) {
		t.Fatalf("List root = %v, want [a b sub/]", root)
	}
	under, err := e.List(ctx, "uv.t", "sub")
	if err != nil {
		t.Fatalf("List sub: %v", err)
	}
	if !reflect.DeepEqual(under, []string{"c"}) {
		t.Fatalf("List sub = %v, want [c]", under)
	}
	// Another token's list is empty.
	other, _ := e.List(ctx, "uv.other", "")
	if len(other) != 0 {
		t.Fatalf("other token List = %v, want empty", other)
	}
}

// TestDestroyRemovesEverything covers revoke-time cleanup, including nested
// paths, and confirms it is scoped to the one token.
func TestDestroyRemovesEverything(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	_ = e.Write(ctx, "uv.t", "a", secret("1"))
	_ = e.Write(ctx, "uv.t", "deep/b", secret("2"))
	_ = e.Write(ctx, "uv.t", "deep/nested/c", secret("3"))
	_ = e.Write(ctx, "uv.survivor", "keep", secret("x"))

	if err := e.Destroy(ctx, "uv.t"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if left, _ := e.List(ctx, "uv.t", ""); len(left) != 0 {
		t.Fatalf("after Destroy, List = %v, want empty", left)
	}
	if _, err := e.Read(ctx, "uv.t", "deep/nested/c"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("nested entry survived Destroy: %v", err)
	}
	// The other token is untouched.
	if got, err := e.Read(ctx, "uv.survivor", "keep"); err != nil || got["value"] != "x" {
		t.Fatalf("Destroy leaked across tokens: got %v err %v", got, err)
	}
	// Destroying a token with nothing stored is a no-op.
	if err := e.Destroy(ctx, "uv.empty"); err != nil {
		t.Fatalf("Destroy empty: %v", err)
	}
}

func TestInvalidPaths(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	for _, p := range []string{"", "/leading", "trailing/"} {
		if err := e.Write(ctx, "uv.t", p, secret("v")); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("Write(%q) = %v, want ErrInvalidPath", p, err)
		}
		if _, err := e.Read(ctx, "uv.t", p); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("Read(%q) = %v, want ErrInvalidPath", p, err)
		}
	}
}

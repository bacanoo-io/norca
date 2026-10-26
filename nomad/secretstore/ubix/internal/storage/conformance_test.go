package storage

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// runBackendConformance exercises the [Backend] contract against any
// implementation. Both the in-memory and file backends run it, so the two are
// verified interchangeable — the Liskov substitution guarantee from
// docs/DESIGN.md §7 turned into an executable check.
func runBackendConformance(t *testing.T, newBackend func(t *testing.T) Backend) {
	t.Helper()
	ctx := context.Background()

	t.Run("GetMissingReturnsNil", func(t *testing.T) {
		b := newBackend(t)
		got, err := b.Get(ctx, "does/not/exist")
		if err != nil {
			t.Fatalf("Get: unexpected error: %v", err)
		}
		if got != nil {
			t.Fatalf("Get missing key: want nil, got %+v", got)
		}
	})

	t.Run("PutThenGet", func(t *testing.T) {
		b := newBackend(t)
		want := []byte("cipher-bytes")
		if err := b.Put(ctx, &Entry{Key: "a/b/c", Value: want}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		got, err := b.Get(ctx, "a/b/c")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got == nil || !reflect.DeepEqual(got.Value, want) {
			t.Fatalf("Get: want %q, got %+v", want, got)
		}
		if got.Key != "a/b/c" {
			t.Fatalf("Get: key = %q, want %q", got.Key, "a/b/c")
		}
	})

	t.Run("StoredValueIsIsolatedFromCaller", func(t *testing.T) {
		b := newBackend(t)
		in := []byte("secret")
		if err := b.Put(ctx, &Entry{Key: "k", Value: in}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		in[0] = 'X' // mutating the caller's slice must not change stored data

		got, err := b.Get(ctx, "k")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if string(got.Value) != "secret" {
			t.Fatalf("stored value changed after caller mutation: %q", got.Value)
		}
		got.Value[0] = 'Y' // mutating a returned slice must not change stored data

		again, err := b.Get(ctx, "k")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if string(again.Value) != "secret" {
			t.Fatalf("stored value changed after returned-slice mutation: %q", again.Value)
		}
	})

	t.Run("PutOverwrites", func(t *testing.T) {
		b := newBackend(t)
		_ = b.Put(ctx, &Entry{Key: "k", Value: []byte("v1")})
		if err := b.Put(ctx, &Entry{Key: "k", Value: []byte("v2")}); err != nil {
			t.Fatalf("Put overwrite: %v", err)
		}
		got, _ := b.Get(ctx, "k")
		if string(got.Value) != "v2" {
			t.Fatalf("overwrite: want v2, got %q", got.Value)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		b := newBackend(t)
		_ = b.Put(ctx, &Entry{Key: "k", Value: []byte("v")})
		if err := b.Delete(ctx, "k"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		got, _ := b.Get(ctx, "k")
		if got != nil {
			t.Fatalf("Delete: key still present: %+v", got)
		}
	})

	t.Run("DeleteMissingIsNoOp", func(t *testing.T) {
		b := newBackend(t)
		if err := b.Delete(ctx, "never/existed"); err != nil {
			t.Fatalf("Delete missing: %v", err)
		}
	})

	t.Run("ListHierarchy", func(t *testing.T) {
		b := newBackend(t)
		for _, k := range []string{"a", "b/c", "b/d", "e/f/g"} {
			if err := b.Put(ctx, &Entry{Key: k, Value: []byte("v")}); err != nil {
				t.Fatalf("Put %q: %v", k, err)
			}
		}
		cases := map[string][]string{
			"":     {"a", "b/", "e/"},
			"b/":   {"c", "d"},
			"e/":   {"f/"},
			"e/f/": {"g"},
			"z/":   {},
		}
		for prefix, want := range cases {
			got, err := b.List(ctx, prefix)
			if err != nil {
				t.Fatalf("List(%q): %v", prefix, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("List(%q) = %v, want %v", prefix, got, want)
			}
		}
	})

	t.Run("RejectsInvalidKeys", func(t *testing.T) {
		b := newBackend(t)
		for _, k := range []string{"", "/a", "a/", "a//b", "a/../b", ".", "..", "a/./b"} {
			if _, err := b.Get(ctx, k); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Get(%q): want ErrInvalidKey, got %v", k, err)
			}
			if err := b.Put(ctx, &Entry{Key: k, Value: []byte("v")}); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Put(%q): want ErrInvalidKey, got %v", k, err)
			}
			if err := b.Delete(ctx, k); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Delete(%q): want ErrInvalidKey, got %v", k, err)
			}
		}
	})

	t.Run("RejectsInvalidListPrefix", func(t *testing.T) {
		b := newBackend(t)
		for _, p := range []string{"a", "/a/", "a//", "a/../"} {
			if _, err := b.List(ctx, p); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("List(%q): want ErrInvalidKey, got %v", p, err)
			}
		}
	})
}

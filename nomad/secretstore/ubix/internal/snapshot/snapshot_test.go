package snapshot

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

func TestWriteRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := storage.NewMemoryBackend()
	want := map[string]string{
		"core/keyring":     "encrypted-keyring",
		"core/seal-config": `{"type":"auto"}`,
		"secret/data/a/1":  "cipher-a",
		"secret/meta/a":    "meta-a",
		"transit/key/k":    "cipher-k",
	}
	for k, v := range want {
		if err := src.Put(ctx, &storage.Entry{Key: k, Value: []byte(v)}); err != nil {
			t.Fatalf("seed %q: %v", k, err)
		}
	}

	var buf bytes.Buffer
	if err := Write(ctx, src, &buf); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.HasPrefix(buf.String(), header) {
		t.Fatalf("snapshot missing header: %q", buf.String()[:20])
	}

	dst := storage.NewMemoryBackend()
	if err := Restore(ctx, dst, &buf); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for k, v := range want {
		got, err := dst.Get(ctx, k)
		if err != nil || got == nil || string(got.Value) != v {
			t.Fatalf("restored %q = %+v, err %v, want %q", k, got, err, v)
		}
	}
}

func TestRestoreRejectsBadHeader(t *testing.T) {
	ctx := context.Background()
	err := Restore(ctx, storage.NewMemoryBackend(), strings.NewReader("not-a-snapshot\n{}"))
	if err == nil {
		t.Fatal("Restore accepted a bad header")
	}
}

func TestWriteEmptyBackend(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	if err := Write(ctx, storage.NewMemoryBackend(), &buf); err != nil {
		t.Fatalf("Write empty: %v", err)
	}
	// Header only; restoring yields nothing.
	dst := storage.NewMemoryBackend()
	if err := Restore(ctx, dst, &buf); err != nil {
		t.Fatalf("Restore empty: %v", err)
	}
}

package barrier

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

func TestRekeyChangesMasterKeyKeepsData(t *testing.T) {
	ctx := context.Background()
	b, _, oldMaster := newInitializedBarrier(t)

	// Write some data under the (unchanged) barrier key.
	if err := b.Put(ctx, &storage.Entry{Key: "app/secret", Value: []byte("s3cr3t")}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	newMaster := newKey(t)
	if err := b.Rekey(ctx, oldMaster, newMaster); err != nil {
		t.Fatalf("Rekey: %v", err)
	}

	// The barrier is still unsealed and serving — data reads back unchanged.
	got, err := b.Get(ctx, "app/secret")
	if err != nil {
		t.Fatalf("Get after rekey: %v", err)
	}
	if !bytes.Equal(got.Value, []byte("s3cr3t")) {
		t.Fatalf("data after rekey = %q, want s3cr3t", got.Value)
	}

	// A fresh barrier over the same store: the OLD master no longer unseals...
	b2 := New(b.phys)
	if err := b2.Unseal(ctx, oldMaster); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("unseal with old master = %v, want ErrInvalidKey", err)
	}
	// ...but the NEW master does, and the data is intact.
	if err := b2.Unseal(ctx, newMaster); err != nil {
		t.Fatalf("unseal with new master: %v", err)
	}
	got, err = b2.Get(ctx, "app/secret")
	if err != nil {
		t.Fatalf("Get from re-unsealed barrier: %v", err)
	}
	if !bytes.Equal(got.Value, []byte("s3cr3t")) {
		t.Fatalf("data = %q, want s3cr3t", got.Value)
	}
}

func TestRekeyWrongOldMasterRejected(t *testing.T) {
	ctx := context.Background()
	b, _, _ := newInitializedBarrier(t)
	if err := b.Rekey(ctx, newKey(t), newKey(t)); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Rekey with wrong old master = %v, want ErrInvalidKey", err)
	}
}

func TestRekeyUninitialized(t *testing.T) {
	ctx := context.Background()
	b := New(storage.NewMemoryBackend())
	if err := b.Rekey(ctx, newKey(t), newKey(t)); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Rekey uninitialized = %v, want ErrNotInitialized", err)
	}
}

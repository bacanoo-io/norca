// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secretstore

import (
	"context"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/secretstore/ubix/export/barrier"
	"github.com/hashicorp/nomad/secretstore/ubix/export/kv"
	"github.com/hashicorp/nomad/secretstore/ubix/export/storage"
	"github.com/shoenig/test/must"
)

func TestBatch_Backend(t *testing.T) {
	ci.Parallel(t)
	b, r := newTestBackend(t)
	r.setLeader(b, true)
	ctx := context.Background()
	must.NoError(t, b.Put(ctx, &storage.Entry{Key: "kept", Value: []byte("old")}))
	must.NoError(t, b.Put(ctx, &storage.Entry{Key: "gone", Value: []byte("old")}))
	applied := len(r.applied)

	bctx, batch := WithBatch(ctx)
	must.True(t, batch.Empty())
	must.NoError(t, b.Put(bctx, &storage.Entry{Key: "new", Value: []byte("n")}))
	must.NoError(t, b.Put(bctx, &storage.Entry{Key: "kept", Value: []byte("k")}))
	must.NoError(t, b.Delete(bctx, "gone"))

	// Nothing went through Raft.
	must.Eq(t, applied, len(r.applied))

	// The batch's context reads its own writes, others read the store.
	e, err := b.Get(bctx, "kept")
	must.NoError(t, err)
	must.Eq(t, []byte("k"), e.Value)
	e, err = b.Get(bctx, "gone")
	must.NoError(t, err)
	must.Nil(t, e)
	e, err = b.Get(ctx, "gone")
	must.NoError(t, err)
	must.Eq(t, []byte("old"), e.Value)
	e, err = b.Get(ctx, "new")
	must.NoError(t, err)
	must.Nil(t, e)

	must.Eq(t, []*structs.SecretStoreBlob{
		{Path: "kept", Value: []byte("k")},
		{Path: "new", Value: []byte("n")},
	}, batch.Blobs())
	must.Eq(t, []string{"gone"}, batch.Deletes())

	// A put after a delete wins, and vice versa.
	must.NoError(t, b.Delete(bctx, "new"))
	must.NoError(t, b.Put(bctx, &storage.Entry{Key: "gone", Value: []byte("back")}))
	must.Eq(t, []string{"new"}, batch.Deletes())
	must.Len(t, 2, batch.Blobs())

	// The key and size checks still apply.
	must.ErrorIs(t, b.Put(bctx, &storage.Entry{Key: "/bad", Value: nil}), storage.ErrInvalidKey)
	must.ErrorIs(t, b.Put(bctx, &storage.Entry{Key: "big", Value: make([]byte, DefaultMaxValueSize+1)}), ErrValueTooLarge)

	// CheckWrite applies the version gate.
	must.NoError(t, b.CheckWrite(ctx))
	r.ready.Store(false)
	must.ErrorIs(t, b.CheckWrite(ctx), ErrServersNotReady)
}

// TestBatch_KV drives the KV engine on an unsealed barrier with a batch: the
// engine reads its own writes, and the collected blobs are ciphertext that
// reads back once applied.
func TestBatch_KV(t *testing.T) {
	ci.Parallel(t)
	b, r := newTestBackend(t)
	r.setLeader(b, true)
	ctx := context.Background()

	bar := barrier.New(b)
	key := make([]byte, barrier.KeySize)
	must.NoError(t, bar.Initialize(ctx, key))
	must.NoError(t, bar.Unseal(ctx, key))
	eng := kv.New(bar, "secrets")

	bctx, batch := WithBatch(ctx)
	_, err := eng.Write(bctx, "ns/app", map[string]any{"pw": "canary-batch"})
	must.NoError(t, err)
	_, err = eng.Write(bctx, "ns/app", map[string]any{"pw": "canary-batch-2"})
	must.NoError(t, err)
	meta, err := eng.ReadMetadata(bctx, "ns/app")
	must.NoError(t, err)
	must.Eq(t, 2, meta.CurrentVersion)

	// Not applied yet.
	_, err = eng.ReadMetadata(ctx, "ns/app")
	must.ErrorIs(t, err, kv.ErrSecretNotFound)

	for _, blob := range batch.Blobs() {
		must.NotEq(t, -1, len(blob.Value))
		must.StrNotContains(t, string(blob.Value), "canary-batch")
	}
	must.NoError(t, r.store.UpsertSecretStoreBlobs(structs.MsgTypeTestSetup, 100, batch.Blobs()))
	data, _, err := eng.Read(ctx, "ns/app", 1)
	must.NoError(t, err)
	must.Eq(t, "canary-batch", data["pw"])

	// Preloaded blobs are read through the barrier like stored ones.
	pctx, pre := WithBatch(ctx)
	pre.Preload([]*structs.SecretStoreBlob{{Path: "copy/x", Value: []byte("not ciphertext")}})
	_, err = bar.Get(pctx, "copy/x")
	must.Error(t, err)
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consulkv

import (
	"context"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"
)

// testBucket starts a NATS server with JetStream and returns a bucket.
func testBucket(t *testing.T) jetstream.KeyValue {
	t.Helper()
	srv, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true, JetStream: true, StoreDir: t.TempDir(),
	})
	must.NoError(t, err)
	go srv.Start()
	must.True(t, srv.ReadyForConnections(10*time.Second))
	t.Cleanup(func() { srv.Shutdown(); srv.WaitForShutdown() })

	nc, err := nats.Connect(srv.ClientURL())
	must.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	must.NoError(t, err)
	kv, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{Bucket: "test"})
	must.NoError(t, err)
	return kv
}

// TestDelete_CAS checks the check-and-set delete. In the Consul KV API a
// delete with cas=0 never deletes an existing key ("the index must be greater
// than 0 for Consul to take any action").
func TestDelete_CAS(t *testing.T) {
	ctx := context.Background()
	kv := testBucket(t)
	revision := func(key string) uint64 {
		t.Helper()
		e, err := kv.Get(ctx, EncodeKey(key))
		must.NoError(t, err)
		return e.Revision()
	}

	ok, err := Put(ctx, kv, "", "app/key", []byte("v"), nil)
	must.NoError(t, err)
	must.True(t, ok)
	rev := revision("app/key")

	// cas=0 leaves an existing key alone.
	zero := uint64(0)
	ok, err = Delete(ctx, kv, "", "app/key", &zero)
	must.NoError(t, err)
	must.False(t, ok, must.Sprint("cas=0 must not delete an existing key"))
	must.Eq(t, rev, revision("app/key"))

	// A stale index does not delete, the current one does.
	stale := rev + 1000
	ok, err = Delete(ctx, kv, "", "app/key", &stale)
	must.NoError(t, err)
	must.False(t, ok)
	must.Eq(t, rev, revision("app/key"))
	ok, err = Delete(ctx, kv, "", "app/key", &rev)
	must.NoError(t, err)
	must.True(t, ok)
	_, err = kv.Get(ctx, EncodeKey("app/key"))
	must.ErrorIs(t, err, jetstream.ErrKeyNotFound)

	// Without cas the key is deleted.
	ok, err = Put(ctx, kv, "", "app/other", []byte("v"), nil)
	must.NoError(t, err)
	must.True(t, ok)
	ok, err = Delete(ctx, kv, "", "app/other", nil)
	must.NoError(t, err)
	must.True(t, ok)
	_, err = kv.Get(ctx, EncodeKey("app/other"))
	must.ErrorIs(t, err, jetstream.ErrKeyNotFound)
}

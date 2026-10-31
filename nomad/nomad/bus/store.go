// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package bus

import (
	"context"
	"errors"
	"fmt"

	"github.com/hashicorp/go-hclog"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// KVEntry is a key of a key-value bucket, with its NATS key.
type KVEntry struct {
	Key   string
	Value []byte
}

// ReadStoreKV reads the keys of a key-value bucket from a JetStream store
// that no server uses, such as the store of a standalone bus kept aside
// (StandaloneStoreDir): it starts a temporary standalone server without
// listeners on the store. A missing store or bucket returns no keys.
func ReadStoreKV(ctx context.Context, storeDir, bucket string, maxStorage int64, logger hclog.Logger) ([]KVEntry, error) {
	opts := &natsserver.Options{
		ServerName:         "norca-bus-store-reader",
		DontListen:         true,
		JetStream:          true,
		StoreDir:           storeDir,
		JetStreamMaxMemory: 32 << 20,
		JetStreamMaxStore:  maxStorage,
		NoSigs:             true,
	}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("bus: opening the store %s: %w", storeDir, err)
	}
	ns.SetLoggerV2(&natsLogger{logger: logger.Named("store-reader")}, false, false, false)
	go ns.Start()
	defer func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	}()
	if !ns.ReadyForConnections(startTimeout) {
		return nil, fmt.Errorf("bus: the store %s did not open within %s", storeDir, startTimeout)
	}

	nc, err := nats.Connect("", nats.InProcessServer(ns), nats.Name("norca-bus-store-reader"))
	if err != nil {
		return nil, err
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, err
	}
	kv, err := js.KeyValue(ctx, bucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	lister, err := kv.ListKeys(ctx)
	if err != nil {
		return nil, err
	}
	// Stopped also on an early return, so the watch of the lister ends.
	defer func() { _ = lister.Stop() }()
	var out []KVEntry
	for key := range lister.Keys() {
		e, err := kv.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("bus: reading %q: %w", key, err)
		}
		out = append(out, KVEntry{Key: key, Value: e.Value()})
	}
	return out, nil
}

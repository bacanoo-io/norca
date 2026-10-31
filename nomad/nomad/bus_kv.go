// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/nomad/helper/consulkv"
	"github.com/hashicorp/nomad/nomad/bus"
	"github.com/nats-io/nats.go/jetstream"
)

// consulKVBucket returns the key-value bucket of the Consul KV on the bus;
// nil, nil when the server runs no bus or the bucket does not exist yet.
func (s *Server) consulKVBucket(ctx context.Context) (jetstream.KeyValue, func(), error) {
	if s.bus == nil {
		return nil, func() {}, nil
	}
	if !s.bus.JetStreamReady() {
		return nil, nil, errors.New("the system bus has no JetStream meta leader")
	}
	nc, err := s.bus.Connect()
	if err != nil {
		return nil, nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, err
	}
	kv, err := js.KeyValue(ctx, bus.ConsulKVBucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		nc.Close()
		return nil, func() {}, nil
	}
	if err != nil {
		nc.Close()
		return nil, nil, err
	}
	return kv, nc.Close, nil
}

// exportConsulKV reads every key of the Consul KV bucket of the bus; nil
// without a bus or bucket.
func (s *Server) exportConsulKV(ctx context.Context) (*consulkv.Export, error) {
	kv, done, err := s.consulKVBucket(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	if kv == nil {
		return nil, nil
	}
	lister, err := kv.ListKeys(ctx)
	if err != nil {
		return nil, err
	}
	out := &consulkv.Export{Bucket: bus.ConsulKVBucket, Entries: []*consulkv.Entry{}}
	for natsKey := range lister.Keys() {
		e, err := kv.Get(ctx, natsKey)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue // deleted meanwhile
		}
		if err != nil {
			return nil, fmt.Errorf("reading %q: %w", natsKey, err)
		}
		key, err := consulkv.DecodeKey(natsKey)
		if err != nil {
			return nil, err
		}
		out.Entries = append(out.Entries, &consulkv.Entry{
			Key: key, Value: e.Value(), Revision: e.Revision(), Created: e.Created().UTC(),
		})
	}
	slices.SortFunc(out.Entries, func(a, b *consulkv.Entry) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}

// backupConsulKV is the ConsulKV dependency of the backups: the export of
// the bucket, or nil without a bus or bucket.
func (s *Server) backupConsulKV(ctx context.Context) ([]byte, error) {
	export, err := s.exportConsulKV(ctx)
	if err != nil || export == nil {
		return nil, err
	}
	return consulkv.Encode(export)
}

// importConsulKV writes keys of the Consul KV bucket of the bus. With replace
// the bucket is emptied first (a full restore).
func (s *Server) importConsulKV(ctx context.Context, entries []*consulkv.Entry, replace bool) error {
	kv, done, err := s.consulKVBucket(ctx)
	if err != nil {
		return err
	}
	defer done()
	if kv == nil {
		return errors.New("the system bus has no Consul KV bucket")
	}
	if replace {
		lister, err := kv.ListKeys(ctx)
		if err != nil {
			return err
		}
		keep := map[string]bool{}
		for _, e := range entries {
			keep[consulkv.EncodeKey(e.Key)] = true
		}
		for k := range lister.Keys() {
			if !keep[k] {
				if err := kv.Delete(ctx, k); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
					return err
				}
			}
		}
	}
	for _, e := range entries {
		if _, err := kv.Put(ctx, consulkv.EncodeKey(e.Key), e.Value); err != nil {
			return fmt.Errorf("writing %q: %w", e.Key, err)
		}
	}
	return nil
}

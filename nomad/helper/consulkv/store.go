// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consulkv

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Timeout bounds an operation on the bucket that does not block.
const Timeout = 10 * time.Second

// The functions below keep the Consul KV in a NATS JetStream key-value
// bucket. The ModifyIndex of a key is its revision (the stream sequence of
// its last change); CreateIndex is not kept by NATS and equals ModifyIndex.
// The blocking index (X-Consul-Index) is the highest revision of the matching
// keys plus one, or the last sequence of the bucket plus one when none
// matches, like the table index of Consul: a later write always raises it,
// also the first write of a fresh bucket (revision 1). They know nothing of
// the Nomad ACLs: the callers check the Variables rules of the keys.

// Get returns the live key, or the keys with the prefix key, and the
// blocking index of the result. With index set it blocks until the blocking
// index exceeds index or wait passes.
func Get(ctx context.Context, kv jetstream.KeyValue, key string, prefix bool, index uint64, wait time.Duration) ([]*Entry, uint64, error) {
	// A watch delivers the last revision of every matching key (delete
	// markers included), a nil entry, then the changes.
	ctx, cancel := context.WithTimeout(ctx, wait+Timeout)
	defer cancel()
	w, err := kv.Watch(ctx, Filter(key, prefix))
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = w.Stop() }()

	live := make(map[string]jetstream.KeyValueEntry)
	var highest uint64 // the highest revision seen; the blocking index is highest+1
	var timeout <-chan time.Time
	initial := true
	for {
		select {
		case e, ok := <-w.Updates():
			if !ok {
				return nil, 0, errors.New("the NATS watch of the KV ended")
			}
			if e == nil {
				initial = false
				if highest == 0 {
					// No matching key: the last sequence of the bucket.
					if st, err := kv.Status(ctx); err == nil {
						if bs, ok := st.(*jetstream.KeyValueBucketStatus); ok {
							highest = bs.StreamInfo().State.LastSeq
						}
					}
				}
				if index == 0 || highest+1 > index {
					return entries(live), highest + 1, nil
				}
				timer := time.NewTimer(wait)
				defer timer.Stop()
				timeout = timer.C
				continue
			}
			k, err := DecodeKey(e.Key())
			if err != nil || (prefix && !strings.HasPrefix(k, key)) || (!prefix && k != key) {
				continue
			}
			highest = max(highest, e.Revision())
			if e.Operation() == jetstream.KeyValuePut {
				live[k] = e
			} else {
				delete(live, k)
			}
			if !initial {
				return entries(live), highest + 1, nil
			}
		case <-timeout:
			return entries(live), highest + 1, nil
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}
}

// entries converts the live keys.
func entries(live map[string]jetstream.KeyValueEntry) []*Entry {
	out := make([]*Entry, 0, len(live))
	for k, e := range live {
		out = append(out, &Entry{Key: k, Value: e.Value(), Revision: e.Revision(), Created: e.Created()})
	}
	return out
}

// Put stores the value of key. With cas set it only stores when the
// revision of the key is *cas (0: the key must not exist) and reports false
// otherwise.
func Put(ctx context.Context, kv jetstream.KeyValue, key string, value []byte, cas *uint64) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	k := EncodeKey(key)
	var err error
	switch {
	case cas == nil:
		_, err = kv.Put(ctx, k, value)
	case *cas == 0:
		_, err = kv.Create(ctx, k, value)
	default:
		_, err = kv.Update(ctx, k, value, *cas)
	}
	if errors.Is(err, jetstream.ErrKeyExists) || errors.Is(err, jetstream.ErrKeyRevisionMismatch) || isWrongLastSequence(err) {
		return false, nil
	}
	return err == nil, err
}

// Delete deletes key. With cas set it only deletes when the revision of the
// key is *cas and reports false otherwise. A tree is deleted by listing its
// keys with Get and deleting them one by one: not atomic.
func Delete(ctx context.Context, kv jetstream.KeyValue, key string, cas *uint64) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	var opts []jetstream.KVDeleteOpt
	if cas != nil {
		opts = append(opts, jetstream.LastRevision(*cas))
	}
	err := kv.Delete(ctx, EncodeKey(key), opts...)
	if errors.Is(err, jetstream.ErrKeyRevisionMismatch) || isWrongLastSequence(err) {
		return false, nil
	}
	return err == nil, err
}

// isWrongLastSequence reports a failed check-and-set of the stream.
func isWrongLastSequence(err error) bool {
	var apiErr *jetstream.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence
}

// Filter returns the subject filter of the watch of a key or a prefix: the
// directory of the prefix and below; the prefix itself is matched on the
// decoded keys.
func Filter(key string, prefix bool) string {
	if !prefix {
		return EncodeKey(key)
	}
	i := strings.LastIndex(key, "/")
	if i < 0 {
		return ">"
	}
	return EncodeKey(key[:i]) + ".>"
}

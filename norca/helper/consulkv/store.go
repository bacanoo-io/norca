// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consulkv

import (
	"context"
	"errors"
	"fmt"
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
//
// scope is the beginning of the NATS keys: ScopePrefix in the bucket of the
// system bus, "" in a bucket of its own.

// Get returns the live key, or the keys with the prefix key, and the
// blocking index of the result. With index set it blocks until the blocking
// index exceeds index or wait passes.
//
// The index of a result without a matching key is the last sequence of the
// bucket, which writes of other keys raise too: a blocking query for a key
// that does not exist returns, unchanged, after any write to the bucket.
// Consul's API allows such returns; the caller queries again.
func Get(ctx context.Context, kv jetstream.KeyValue, scope, key string, prefix bool, index uint64, wait time.Duration) ([]*Entry, uint64, error) {
	// A watch delivers the last revision of every matching key (delete
	// markers included), a nil entry, then the changes.
	ctx, cancel := context.WithTimeout(ctx, wait+Timeout)
	defer cancel()

	// The last sequence of the bucket, the index of a result without a
	// matching key, is read before the watch starts: whatever is written
	// after it is in what the watch delivers. Read after the first values of
	// the watch, it covered a write the result did not have (a put of the
	// key between the two): the caller got no key with an index past its
	// put, and its next blocking query saw the key only when its wait time
	// was over.
	//
	// Norca: a failure to read it fails the query. Taken for 0, it gave the
	// index 1, below an index the caller had already got (review finding
	// HLP-9).
	st, err := kv.Status(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("reading the status of the KV: %w", err)
	}
	bs, ok := st.(*jetstream.KeyValueBucketStatus)
	if !ok {
		return nil, 0, fmt.Errorf("the status of the KV is a %T, not a bucket status", st)
	}
	lastSeq := bs.StreamInfo().State.LastSeq

	w, err := kv.Watch(ctx, scope+Filter(key, prefix))
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
					highest = lastSeq
				}
				if index == 0 || highest+1 > index {
					return entries(live), highest + 1, nil
				}
				timer := time.NewTimer(wait)
				defer timer.Stop()
				timeout = timer.C
				continue
			}
			natsKey, ok := strings.CutPrefix(e.Key(), scope)
			if !ok {
				continue
			}
			k, err := DecodeKey(natsKey)
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
func Put(ctx context.Context, kv jetstream.KeyValue, scope, key string, value []byte, cas *uint64) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	k := scope + EncodeKey(key)
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
// key is *cas and reports ok false otherwise; a cas of 0 never deletes (in
// the Consul KV API the index of a delete must be greater than 0 for any
// action). A delete of a key that does not exist is ok, with and without
// cas: there is nothing to delete.
//
// deleted reports whether this call deleted the key, which ok does not say:
// only then is there a change to announce. A tree is deleted by listing its
// keys with Get and deleting them one by one: not atomic.
func Delete(ctx context.Context, kv jetstream.KeyValue, scope, key string, cas *uint64) (ok, deleted bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	k := scope + EncodeKey(key)
	// The bucket takes the delete of a missing key like any other (it
	// stores a marker), and answers a delete with a revision of a missing
	// key like one with a stale revision, so the key is looked up first. A
	// key deleted between the two calls is answered as stale with cas, and
	// as deleted here without.
	switch _, err := kv.Get(ctx, k); {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return true, false, nil
	case err != nil:
		return false, false, err
	}
	var opts []jetstream.KVDeleteOpt
	if cas != nil {
		if *cas == 0 {
			// The bucket would take revision 0 as "no check" and delete.
			return false, false, nil
		}
		opts = append(opts, jetstream.LastRevision(*cas))
	}
	err = kv.Delete(ctx, k, opts...)
	if errors.Is(err, jetstream.ErrKeyRevisionMismatch) || isWrongLastSequence(err) {
		return false, false, nil
	}
	return err == nil, err == nil, err
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

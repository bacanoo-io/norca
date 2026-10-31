// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consulcompat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/helper/consulkv"
	structsc "github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// natsTimeout bounds a NATS operation that does not block.
const natsTimeout = 10 * time.Second

// natsKV keeps the Consul KV in a NATS JetStream key-value bucket: of an
// external NATS (backend "nats") or of the system bus of the servers
// (backend "bus"). The ModifyIndex of a key is its revision (the stream
// sequence of its last change); CreateIndex is not kept by NATS and equals
// ModifyIndex. The blocking index (X-Consul-Index) is the highest revision
// of the matching keys plus one, or the last sequence of the bucket plus one
// when none matches, like the table index of Consul: a later write always
// raises it, also the first write of a fresh bucket (revision 1). Flags are not
// supported. NATS does not know the Nomad ACLs, so the Variables rules of
// the keys are checked here.
type natsKV struct {
	logger hclog.Logger
	acl    *kvACL

	// name is the bucket; create and replicas create it when it is missing
	// (backend "nats" with create_bucket; the bus bucket is created by the
	// servers).
	name     string
	create   bool
	replicas int

	// connect opens the connection; shared connections (the bus connection
	// of the client) are not closed by Close.
	connect func() (*nats.Conn, error)
	shared  bool

	mu sync.Mutex
	nc *nats.Conn
	kv jetstream.KeyValue
}

func newNATSKV(logger hclog.Logger, cfg *structsc.ConsulCompatKVNATSConfig, acl *kvACL) *natsKV {
	return &natsKV{
		logger:   logger.Named("kv"),
		acl:      acl,
		name:     cfg.BucketName(),
		create:   cfg.CreatesBucket(),
		replicas: max(cfg.Replicas, 1),
		connect:  func() (*nats.Conn, error) { return dialNATS(cfg) },
	}
}

// newBusKV keeps the Consul KV in the bucket of the system bus, on the bus
// connection of the client.
func newBusKV(logger hclog.Logger, connect func() (*nats.Conn, error), acl *kvACL) *natsKV {
	return &natsKV{
		logger:  logger.Named("kv"),
		acl:     acl,
		name:    structsc.DefaultConsulCompatKVBucket,
		connect: connect,
		shared:  true,
	}
}

// dialNATS connects to an external NATS with the credentials of the nats
// block.
func dialNATS(c *structsc.ConsulCompatKVNATSConfig) (*nats.Conn, error) {
	opts := []nats.Option{nats.Name("norca-consul-compat-kv"), nats.MaxReconnects(-1)}
	switch {
	case c.CredentialsFile != "":
		opts = append(opts, nats.UserCredentials(c.CredentialsFile))
	case c.NKeySeedFile != "":
		opt, err := nats.NkeyOptionFromSeed(c.NKeySeedFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read the nkey seed: %w", err)
		}
		opts = append(opts, opt)
	case c.User != "":
		opts = append(opts, nats.UserInfo(c.User, c.Password))
	case c.Token != "":
		opts = append(opts, nats.Token(c.Token))
	}
	if c.CAFile != "" {
		opts = append(opts, nats.RootCAs(c.CAFile))
	}
	if c.CertFile != "" {
		opts = append(opts, nats.ClientCert(c.CertFile, c.KeyFile))
	}
	return nats.Connect(strings.Join(c.URLs, ","), opts...)
}

// bucket returns the bucket, connecting on first use: the agent starts
// without NATS, and a lost connection is reestablished by the client.
func (n *natsKV) bucket() (jetstream.KeyValue, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.kv != nil {
		return n.kv, nil
	}

	if n.nc == nil {
		nc, err := n.connect()
		if err != nil {
			return nil, &codedError{http.StatusServiceUnavailable, fmt.Sprintf("NATS backend of the KV is not available: %v", err)}
		}
		n.nc = nc
	}

	js, err := jetstream.New(n.nc)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), natsTimeout)
	defer cancel()
	name := n.name
	kv, err := js.KeyValue(ctx, name)
	if errors.Is(err, jetstream.ErrBucketNotFound) && n.create {
		kv, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:      name,
			Description: "Consul KV of the Consul-compatible API of Norca",
			History:     1,
			Replicas:    n.replicas,
		})
		if errors.Is(err, jetstream.ErrBucketExists) {
			kv, err = js.KeyValue(ctx, name)
		}
		if err == nil {
			n.logger.Info("created the NATS key-value bucket of the Consul KV", "bucket", name)
		}
	}
	if err != nil {
		return nil, &codedError{http.StatusServiceUnavailable, fmt.Sprintf("NATS bucket %q of the KV is not available: %v", name, err)}
	}
	n.kv = kv
	return kv, nil
}

func (n *natsKV) Get(r *request, key string, prefix bool) ([]*kvEntry, uint64, error) {
	op := acl.VariablesCapabilityRead
	if prefix {
		op = acl.VariablesCapabilityList
	}
	if err := n.acl.allow(r, op, key); err != nil {
		return nil, 0, err
	}
	obj, err := n.acl.backend.ResolveACL(r.token)
	if err != nil {
		return nil, 0, err
	}

	kv, err := n.bucket()
	if err != nil {
		return nil, 0, err
	}

	// A watch delivers the last revision of every matching key (delete
	// markers included), a nil entry, then the changes.
	ctx, cancel := context.WithTimeout(context.Background(), r.wait+natsTimeout)
	defer cancel()
	w, err := kv.Watch(ctx, natsFilter(key, prefix))
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = w.Stop() }()

	live := make(map[string]jetstream.KeyValueEntry)
	var index uint64 // the highest revision seen; the blocking index is index+1
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
				if index == 0 {
					// No matching key: the last sequence of the bucket.
					if st, err := kv.Status(ctx); err == nil {
						if bs, ok := st.(*jetstream.KeyValueBucketStatus); ok {
							index = bs.StreamInfo().State.LastSeq
						}
					}
				}
				if r.index == 0 || index+1 > r.index {
					return natsEntries(live, n.acl, obj), index + 1, nil
				}
				timer := time.NewTimer(r.wait)
				defer timer.Stop()
				timeout = timer.C
				continue
			}
			k, err := decodeNATSKey(e.Key())
			if err != nil || (prefix && !strings.HasPrefix(k, key)) || (!prefix && k != key) {
				continue
			}
			index = max(index, e.Revision())
			if e.Operation() == jetstream.KeyValuePut {
				live[k] = e
			} else {
				delete(live, k)
			}
			if !initial {
				return natsEntries(live, n.acl, obj), index + 1, nil
			}
		case <-timeout:
			return natsEntries(live, n.acl, obj), index + 1, nil
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}
}

// natsEntries converts the live keys that obj may read.
func natsEntries(live map[string]jetstream.KeyValueEntry, a *kvACL, obj *acl.ACL) []*kvEntry {
	entries := make([]*kvEntry, 0, len(live))
	for k, e := range live {
		if !a.allowed(obj, acl.VariablesCapabilityRead, k) {
			continue
		}
		entries = append(entries, &kvEntry{Key: k, Value: e.Value(), CreateIndex: e.Revision(), ModifyIndex: e.Revision()})
	}
	return entries
}

func (n *natsKV) Put(r *request, e *kvEntry, cas *uint64) (bool, error) {
	if e.Flags != 0 {
		return false, badRequest("flags are not supported by the nats backend of the KV")
	}
	if err := n.acl.allow(r, acl.VariablesCapabilityWrite, e.Key); err != nil {
		return false, err
	}
	kv, err := n.bucket()
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), natsTimeout)
	defer cancel()
	k := encodeNATSKey(e.Key)
	switch {
	case cas == nil:
		_, err = kv.Put(ctx, k, e.Value)
	case *cas == 0:
		_, err = kv.Create(ctx, k, e.Value)
	default:
		_, err = kv.Update(ctx, k, e.Value, *cas)
	}
	if errors.Is(err, jetstream.ErrKeyExists) || errors.Is(err, jetstream.ErrKeyRevisionMismatch) || isWrongLastSequence(err) {
		return false, nil
	}
	return err == nil, err
}

func (n *natsKV) Delete(r *request, key string, recurse bool, cas *uint64) (bool, error) {
	if !recurse {
		if err := n.acl.allow(r, acl.VariablesCapabilityDestroy, key); err != nil {
			return false, err
		}
		kv, err := n.bucket()
		if err != nil {
			return false, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), natsTimeout)
		defer cancel()
		var opts []jetstream.KVDeleteOpt
		if cas != nil {
			opts = append(opts, jetstream.LastRevision(*cas))
		}
		err = kv.Delete(ctx, encodeNATSKey(key), opts...)
		if errors.Is(err, jetstream.ErrKeyRevisionMismatch) || isWrongLastSequence(err) {
			return false, nil
		}
		return err == nil, err
	}

	// Not atomic: the keys are deleted one by one.
	nr := *r
	nr.index = 0
	entries, _, err := n.Get(&nr, key, true)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if _, err := n.Delete(r, e.Key, false, nil); err != nil {
			return false, err
		}
	}
	return true, nil
}

// isWrongLastSequence reports a failed check-and-set of the stream.
func isWrongLastSequence(err error) bool {
	var apiErr *jetstream.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence
}

func (n *natsKV) Close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.nc != nil && !n.shared {
		n.nc.Close()
	}
	n.nc, n.kv = nil, nil
}

// encodeNATSKey and decodeNATSKey map Consul keys to NATS keys and back
// (helper/consulkv, shared with the backups of the servers).
func encodeNATSKey(key string) string { return consulkv.EncodeKey(key) }

func decodeNATSKey(k string) (string, error) { return consulkv.DecodeKey(k) }

// natsFilter returns the subject filter of the watch of a key or a prefix:
// the directory of the prefix and below; the prefix itself is matched on
// the decoded keys.
func natsFilter(key string, prefix bool) string {
	if !prefix {
		return encodeNATSKey(key)
	}
	i := strings.LastIndex(key, "/")
	if i < 0 {
		return ">"
	}
	return encodeNATSKey(key[:i]) + ".>"
}

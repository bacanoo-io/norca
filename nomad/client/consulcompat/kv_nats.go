// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consulcompat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/acl"
	structsc "github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// natsTimeout bounds a NATS operation that does not block.
const natsTimeout = 10 * time.Second

// natsKV keeps the Consul KV in a NATS JetStream key-value bucket. The
// ModifyIndex of a key is its revision (the stream sequence of its last
// change), which also orders the blocking queries; CreateIndex is not kept
// by NATS and equals ModifyIndex. Flags are not supported. NATS does not
// know the Nomad ACLs, so the Variables rules of the keys are checked here.
type natsKV struct {
	logger hclog.Logger
	cfg    *structsc.ConsulCompatKVNATSConfig
	acl    *kvACL

	mu sync.Mutex
	nc *nats.Conn
	kv jetstream.KeyValue
}

func newNATSKV(logger hclog.Logger, cfg *structsc.ConsulCompatKVNATSConfig, acl *kvACL) *natsKV {
	return &natsKV{logger: logger.Named("kv"), cfg: cfg, acl: acl}
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
		opts := []nats.Option{nats.Name("norca-consul-compat-kv"), nats.MaxReconnects(-1)}
		c := n.cfg
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
		nc, err := nats.Connect(strings.Join(c.URLs, ","), opts...)
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
	name := n.cfg.BucketName()
	kv, err := js.KeyValue(ctx, name)
	if errors.Is(err, jetstream.ErrBucketNotFound) && n.cfg.CreatesBucket() {
		kv, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:      name,
			Description: "Consul KV of the Consul-compatible API of Norca",
			History:     1,
			Replicas:    max(n.cfg.Replicas, 1),
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
	var index uint64
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
				if r.index == 0 || index > r.index {
					return natsEntries(live, n.acl, obj), index, nil
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
				return natsEntries(live, n.acl, obj), index, nil
			}
		case <-timeout:
			return natsEntries(live, n.acl, obj), index, nil
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
	if n.nc != nil {
		n.nc.Close()
		n.nc, n.kv = nil, nil
	}
}

// The keys of the Consul KV are mapped to NATS keys: the segments ("/")
// become the tokens of the subject ("."), every byte but letters, digits,
// "-" and "_" becomes "=XX" (its hexadecimal value) and an empty segment
// becomes "=", so every key has a valid NATS key and the keys below a
// directory are matched by "<directory>.>".

func encodeNATSKey(key string) string {
	segments := strings.Split(key, "/")
	for i, seg := range segments {
		if seg == "" {
			segments[i] = "="
			continue
		}
		var b strings.Builder
		for j := 0; j < len(seg); j++ {
			c := seg[j]
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
				b.WriteByte(c)
			} else {
				fmt.Fprintf(&b, "=%02X", c)
			}
		}
		segments[i] = b.String()
	}
	return strings.Join(segments, ".")
}

func decodeNATSKey(k string) (string, error) {
	tokens := strings.Split(k, ".")
	for i, tok := range tokens {
		if tok == "=" {
			tokens[i] = ""
			continue
		}
		var b strings.Builder
		for j := 0; j < len(tok); j++ {
			if tok[j] != '=' {
				b.WriteByte(tok[j])
				continue
			}
			if j+2 >= len(tok) {
				return "", fmt.Errorf("invalid escape in %q", k)
			}
			v, err := strconv.ParseUint(tok[j+1:j+3], 16, 8)
			if err != nil {
				return "", fmt.Errorf("invalid escape in %q", k)
			}
			b.WriteByte(byte(v))
			j += 2
		}
		tokens[i] = b.String()
	}
	return strings.Join(tokens, "/"), nil
}

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

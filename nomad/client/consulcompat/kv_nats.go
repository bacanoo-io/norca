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

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/helper/consulkv"
	structsc "github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// natsKV keeps the Consul KV in a NATS JetStream key-value bucket of an
// external NATS (backend "nats"), with the semantics of helper/consulkv:
// the ModifyIndex of a key is its revision, CreateIndex equals ModifyIndex
// and flags are not supported. NATS does not know the Nomad ACLs, so the
// Variables rules of the keys are checked here.
type natsKV struct {
	logger hclog.Logger
	acl    *kvACL
	cfg    *structsc.ConsulCompatKVNATSConfig

	mu sync.Mutex
	nc *nats.Conn
	kv jetstream.KeyValue
}

func newNATSKV(logger hclog.Logger, cfg *structsc.ConsulCompatKVNATSConfig, acl *kvACL) *natsKV {
	return &natsKV{logger: logger.Named("kv"), acl: acl, cfg: cfg}
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
		nc, err := dialNATS(n.cfg)
		if err != nil {
			return nil, &codedError{http.StatusServiceUnavailable, fmt.Sprintf("NATS backend of the KV is not available: %v", err)}
		}
		n.nc = nc
	}

	js, err := jetstream.New(n.nc)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), consulkv.Timeout)
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
	found, index, err := consulkv.Get(context.Background(), kv, key, prefix, r.index, r.wait)
	if err != nil {
		return nil, 0, err
	}
	// The keys obj may read.
	entries := make([]*kvEntry, 0, len(found))
	for _, e := range found {
		if n.acl.allowed(obj, acl.VariablesCapabilityRead, e.Key) {
			entries = append(entries, &kvEntry{Key: e.Key, Value: e.Value, CreateIndex: e.Revision, ModifyIndex: e.Revision})
		}
	}
	return entries, index, nil
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
	return consulkv.Put(context.Background(), kv, e.Key, e.Value, cas)
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
		return consulkv.Delete(context.Background(), kv, key, cas)
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

func (n *natsKV) Close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.nc != nil {
		n.nc.Close()
	}
	n.nc, n.kv = nil, nil
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consulcompat_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	ctdep "github.com/hashicorp/consul-template/dependency"
	consulapi "github.com/hashicorp/consul/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/structs"
	structsc "github.com/hashicorp/nomad/nomad/structs/config"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/shoenig/test/must"
)

// natsKVConfig starts a NATS server with JetStream and returns the kv block
// of the nats backend.
func natsKVConfig(t *testing.T) *structsc.ConsulCompatKVConfig {
	t.Helper()
	srv, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true, JetStream: true, StoreDir: t.TempDir(),
	})
	must.NoError(t, err)
	go srv.Start()
	must.True(t, srv.ReadyForConnections(10*time.Second))
	t.Cleanup(func() { srv.Shutdown(); srv.WaitForShutdown() })
	return &structsc.ConsulCompatKVConfig{
		Backend: structsc.ConsulCompatKVNATS,
		NATS:    &structsc.ConsulCompatKVNATSConfig{URLs: []string{srv.ClientURL()}},
	}
}

// TestConsulCompat_KV runs the KV of the official Consul API client against
// both backends.
func TestConsulCompat_KV(t *testing.T) {
	ci.Parallel(t)

	backends := map[string]func(t *testing.T) *structsc.ConsulCompatKVConfig{
		"variables": func(*testing.T) *structsc.ConsulCompatKVConfig { return nil },
		"nats":      natsKVConfig,
	}
	for name, kvConfig := range backends {
		t.Run(name, func(t *testing.T) {
			c := newCluster(t, false, &structsc.ConsulCompatConfig{KV: kvConfig(t)})
			kv := c.consul.KV()

			// missing key: 404, nil pair
			pair, _, err := kv.Get("app/config", nil)
			must.NoError(t, err)
			must.Nil(t, pair)

			// put and get; keys with characters outside the Variables paths
			for _, key := range []string{"app/config", "app/db.url", "app/with space", "app/sub/", "app/sub/x", "apps"} {
				_, err := kv.Put(&consulapi.KVPair{Key: key, Value: []byte("v:" + key)}, nil)
				must.NoError(t, err)
			}
			pair, meta, err := kv.Get("app/db.url", nil)
			must.NoError(t, err)
			must.NotNil(t, pair)
			must.Eq(t, "app/db.url", pair.Key)
			must.Eq(t, "v:app/db.url", string(pair.Value))
			must.Positive(t, pair.ModifyIndex)
			must.Positive(t, meta.LastIndex)

			// binary values
			binary := []byte{0, 0xff, 0xfe, 'x'}
			_, err = kv.Put(&consulapi.KVPair{Key: "bin", Value: binary}, nil)
			must.NoError(t, err)
			pair, _, err = kv.Get("bin", nil)
			must.NoError(t, err)
			must.Eq(t, binary, pair.Value)

			// raw
			resp, err := http.Get("http://" + c.addr + "/v1/kv/app/config?raw")
			must.NoError(t, err)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			must.Eq(t, "v:app/config", string(body))

			// recurse: the prefix is a string prefix ("app" matches "apps")
			pairs, _, err := kv.List("app/", nil)
			must.NoError(t, err)
			var keys []string
			for _, p := range pairs {
				keys = append(keys, p.Key)
			}
			must.Eq(t, []string{"app/config", "app/db.url", "app/sub/", "app/sub/x", "app/with space"}, keys)
			pairs, _, err = kv.List("app", nil)
			must.NoError(t, err)
			must.Len(t, 6, pairs)

			// keys with a separator
			names, _, err := kv.Keys("app/", "/", nil)
			must.NoError(t, err)
			must.Eq(t, []string{"app/config", "app/db.url", "app/sub/", "app/with space"}, names)

			// check-and-set
			pair, _, err = kv.Get("app/config", nil)
			must.NoError(t, err)
			ok, _, err := kv.CAS(&consulapi.KVPair{Key: "app/config", Value: []byte("stale"), ModifyIndex: pair.ModifyIndex + 1000}, nil)
			must.NoError(t, err)
			must.False(t, ok)
			ok, _, err = kv.CAS(&consulapi.KVPair{Key: "app/config", Value: []byte("v2"), ModifyIndex: pair.ModifyIndex}, nil)
			must.NoError(t, err)
			must.True(t, ok)
			ok, _, err = kv.CAS(&consulapi.KVPair{Key: "app/config", Value: []byte("create"), ModifyIndex: 0}, nil)
			must.NoError(t, err)
			must.False(t, ok, must.Sprint("cas=0 must not overwrite an existing key"))
			ok, _, err = kv.CAS(&consulapi.KVPair{Key: "app/new", Value: []byte("created"), ModifyIndex: 0}, nil)
			must.NoError(t, err)
			must.True(t, ok)

			// a blocking query returns when the key changes
			pair, meta, err = kv.Get("app/config", nil)
			must.NoError(t, err)
			done := make(chan *consulapi.KVPair, 1)
			go func() {
				p, _, _ := kv.Get("app/config", &consulapi.QueryOptions{WaitIndex: meta.LastIndex, WaitTime: 20 * time.Second})
				done <- p
			}()
			time.Sleep(300 * time.Millisecond)
			_, err = kv.Put(&consulapi.KVPair{Key: "app/config", Value: []byte("v3")}, nil)
			must.NoError(t, err)
			select {
			case p := <-done:
				must.NotNil(t, p)
				must.Eq(t, "v3", string(p.Value))
			case <-time.After(15 * time.Second):
				t.Fatal("the blocking query did not return after the change")
			}

			// delete with cas, delete, delete recurse
			pair, _, err = kv.Get("app/new", nil)
			must.NoError(t, err)
			ok, _, err = kv.DeleteCAS(&consulapi.KVPair{Key: "app/new", ModifyIndex: pair.ModifyIndex + 1000}, nil)
			must.NoError(t, err)
			must.False(t, ok)
			ok, _, err = kv.DeleteCAS(&consulapi.KVPair{Key: "app/new", ModifyIndex: pair.ModifyIndex}, nil)
			must.NoError(t, err)
			must.True(t, ok)
			_, err = kv.Delete("bin", nil)
			must.NoError(t, err)
			pair, _, err = kv.Get("bin", nil)
			must.NoError(t, err)
			must.Nil(t, pair)
			_, err = kv.DeleteTree("app/", nil)
			must.NoError(t, err)
			pairs, _, err = kv.List("", nil)
			must.NoError(t, err)
			must.Len(t, 1, pairs)
			must.Eq(t, "apps", pairs[0].Key)

			// consul-template's key function
			d, err := ctdep.NewKVGetQuery("apps")
			must.NoError(t, err)
			clients := ctdep.NewClientSet()
			must.NoError(t, clients.CreateConsulClient(&ctdep.CreateConsulClientInput{Address: c.addr}))
			v, _, err := d.Fetch(clients, &ctdep.QueryOptions{})
			must.NoError(t, err)
			must.Eq(t, "v:apps", v.(string))

			// sessions are not supported
			_, _, err = kv.Acquire(&consulapi.KVPair{Key: "lock", Session: "s"}, nil)
			must.ErrorContains(t, err, "501")
		})
	}
}

// TestConsulCompat_KVFlags: the variables backend keeps the flags, the nats
// backend refuses them.
func TestConsulCompat_KVFlags(t *testing.T) {
	ci.Parallel(t)

	c := newCluster(t, false, nil)
	_, err := c.consul.KV().Put(&consulapi.KVPair{Key: "f", Flags: 42, Value: []byte("x")}, nil)
	must.NoError(t, err)
	pair, _, err := c.consul.KV().Get("f", nil)
	must.NoError(t, err)
	must.Eq(t, uint64(42), pair.Flags)

	n := newCluster(t, false, &structsc.ConsulCompatConfig{KV: natsKVConfig(t)})
	_, err = n.consul.KV().Put(&consulapi.KVPair{Key: "f", Flags: 42, Value: []byte("x")}, nil)
	must.ErrorContains(t, err, "flags are not supported")
}

// TestConsulCompat_KVACL: both backends apply the Variables rules of the
// path of the keys to the token of the request.
func TestConsulCompat_KVACL(t *testing.T) {
	ci.Parallel(t)

	backends := map[string]func(t *testing.T) *structsc.ConsulCompatKVConfig{
		"variables": func(*testing.T) *structsc.ConsulCompatKVConfig { return nil },
		"nats":      natsKVConfig,
	}
	for name, kvConfig := range backends {
		t.Run(name, func(t *testing.T) {
			c := newCluster(t, true, &structsc.ConsulCompatConfig{KV: kvConfig(t)})

			// read-only below consul/kv/app
			policy := &structs.ACLPolicy{Name: "kv-app-" + name, Rules: `
namespace "default" {
  variables {
    path "consul/kv/app/*" { capabilities = ["read", "list"] }
  }
}`}
			policy.SetHash()
			c.index++
			must.NoError(t, c.server.State().UpsertACLPolicies(structs.MsgTypeTestSetup, c.index, []*structs.ACLPolicy{policy}))
			token := &structs.ACLToken{AccessorID: uuid.Generate(), SecretID: uuid.Generate(), Type: structs.ACLClientToken, Policies: []string{policy.Name}}
			token.SetHash()
			c.index++
			must.NoError(t, c.server.State().UpsertACLTokens(structs.MsgTypeTestSetup, c.index, []*structs.ACLToken{token}))

			root := &consulapi.WriteOptions{Token: c.root.SecretID}
			_, err := c.consul.KV().Put(&consulapi.KVPair{Key: "app/x", Value: []byte("1")}, root)
			must.NoError(t, err)
			_, err = c.consul.KV().Put(&consulapi.KVPair{Key: "other", Value: []byte("2")}, root)
			must.NoError(t, err)

			reader := &consulapi.QueryOptions{Token: token.SecretID}
			pair, _, err := c.consul.KV().Get("app/x", reader)
			must.NoError(t, err)
			must.Eq(t, "1", string(pair.Value))

			_, _, err = c.consul.KV().Get("other", reader)
			must.ErrorContains(t, err, "403")

			_, err = c.consul.KV().Put(&consulapi.KVPair{Key: "app/x", Value: []byte("3")}, &consulapi.WriteOptions{Token: token.SecretID})
			must.ErrorContains(t, err, "403")

			// without a token (and no anonymous token): denied
			_, _, err = c.consul.KV().Get("app/x", nil)
			must.Error(t, err)
			must.True(t, strings.Contains(err.Error(), "403"), must.Sprint(err))
		})
	}
}

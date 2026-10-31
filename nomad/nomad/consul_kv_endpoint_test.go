// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"net/http"
	"sort"
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

// consulKVTest calls the ConsulKV endpoint of a server.
type consulKVTest struct {
	t   *testing.T
	srv *Server
}

func (c *consulKVTest) get(token, namespace, key string, prefix bool, index uint64) (*structs.ConsulKVGetResponse, error) {
	var resp structs.ConsulKVGetResponse
	err := msgpackrpc.CallWithCodec(rpcClient(c.t, c.srv), structs.ConsulKVGetRPCMethod, &structs.ConsulKVGetRequest{
		Key: key, Prefix: prefix,
		QueryOptions: structs.QueryOptions{Region: "global", Namespace: namespace, AuthToken: token,
			MinQueryIndex: index, MaxQueryTime: 20 * time.Second},
	}, &resp)
	return &resp, err
}

func (c *consulKVTest) put(token, namespace, key, value string, cas *uint64) (bool, error) {
	var resp structs.ConsulKVWriteResponse
	err := msgpackrpc.CallWithCodec(rpcClient(c.t, c.srv), structs.ConsulKVPutRPCMethod, &structs.ConsulKVPutRequest{
		Key: key, Value: []byte(value), CAS: cas,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: namespace, AuthToken: token},
	}, &resp)
	return resp.Ok, err
}

func (c *consulKVTest) delete(token, namespace, key string, recurse bool, cas *uint64) (bool, error) {
	var resp structs.ConsulKVWriteResponse
	err := msgpackrpc.CallWithCodec(rpcClient(c.t, c.srv), structs.ConsulKVDeleteRPCMethod, &structs.ConsulKVDeleteRequest{
		Key: key, Recurse: recurse, CAS: cas,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: namespace, AuthToken: token},
	}, &resp)
	return resp.Ok, err
}

// waitBucket waits until the leader created the bucket of the Consul KV.
func (c *consulKVTest) waitBucket(token string) {
	waitBus(c.t, func() (bool, error) {
		_, err := c.get(token, "default", "probe", false, 0)
		return err == nil, err
	})
}

func keysOf(resp *structs.ConsulKVGetResponse) []string {
	keys := []string{}
	for _, e := range resp.Entries {
		keys = append(keys, e.Key)
	}
	sort.Strings(keys)
	return keys
}

// TestConsulKVEndpoint_ACL checks the Variables rules of the keys: read for
// a key, list for a prefix (the listed keys filtered by read), write for a
// put, destroy for a delete, in the namespace of the request; the anonymous
// policy without a token; no access with a node secret.
func TestConsulKVEndpoint_ACL(t *testing.T) {
	ci.Parallel(t)

	s1, root, cleanup := TestACLServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	st := s1.fsm.State()
	c := &consulKVTest{t: t, srv: s1}
	c.waitBucket(root.SecretID)

	policy := func(name, rules string) *structs.ACLPolicy {
		p := &structs.ACLPolicy{Name: name, Rules: rules}
		p.SetHash()
		return p
	}
	index := uint64(1010)
	token := func(policies ...string) string {
		index++
		tok := &structs.ACLToken{AccessorID: uuid.Generate(), SecretID: uuid.Generate(), Type: structs.ACLClientToken, Policies: policies}
		tok.SetHash()
		must.NoError(t, st.UpsertACLTokens(structs.MsgTypeTestSetup, index, []*structs.ACLToken{tok}))
		return tok.SecretID
	}
	must.NoError(t, st.UpsertACLPolicies(structs.MsgTypeTestSetup, 1000, []*structs.ACLPolicy{
		policy("reader", `namespace "default" { variables { path "consul/kv/app/*" { capabilities = ["read", "list"] } } }`),
		policy("lister", `namespace "default" { variables {
			path "consul/kv/*" { capabilities = ["list"] }
			path "consul/kv/app/*" { capabilities = ["read"] } } }`),
		policy("writer-x", `namespace "default" { variables { path "consul/kv/app/*" { capabilities = ["write"] } } }`),
		policy("destroyer-xyz", `namespace "default" { variables { path "consul/kv/app/*" { capabilities = ["read", "list", "destroy"] } } }`),
		policy("other-namespace-writer", `namespace "other" { variables { path "consul/kv/*" { capabilities = ["write", "read"] } } }`),
		policy("anonymous", `namespace "default" { variables { path "consul/kv/public/*" { capabilities = ["read"] } } }`),
	}))
	reader := token("reader")
	lister := token("lister")
	writer := token("writer-x")
	destroyer := token("destroyer-xyz")
	otherNS := token("other-namespace-writer")

	for key, value := range map[string]string{"app/x": "1", "app/y": "2", "other": "3", "public/p": "4"} {
		ok, err := c.put(root.SecretID, "default", key, value, nil)
		must.NoError(t, err)
		must.True(t, ok)
	}

	// read
	resp, err := c.get(reader, "default", "app/x", false, 0)
	must.NoError(t, err)
	must.Eq(t, []string{"app/x"}, keysOf(resp))
	must.Eq(t, "1", string(resp.Entries[0].Value))
	_, err = c.get(reader, "default", "other", false, 0)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	_, err = c.get(writer, "default", "app/x", false, 0)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())

	// list: on the prefix, filtered by read
	resp, err = c.get(reader, "default", "app/", true, 0)
	must.NoError(t, err)
	must.Eq(t, []string{"app/x", "app/y"}, keysOf(resp))
	_, err = c.get(reader, "default", "", true, 0)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	resp, err = c.get(lister, "default", "", true, 0)
	must.NoError(t, err)
	must.Eq(t, []string{"app/x", "app/y"}, keysOf(resp))

	// write
	_, err = c.put(reader, "default", "app/x", "w", nil)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	ok, err := c.put(writer, "default", "app/x", "w", nil)
	must.NoError(t, err)
	must.True(t, ok)
	_, err = c.put(writer, "default", "other", "w", nil)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())

	// destroy
	_, err = c.delete(writer, "default", "app/y", false, nil)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	_, err = c.delete(destroyer, "default", "", true, nil)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	ok, err = c.delete(destroyer, "default", "app/y", false, nil)
	must.NoError(t, err)
	must.True(t, ok)
	ok, err = c.delete(destroyer, "default", "app/", true, nil)
	must.NoError(t, err)
	must.True(t, ok)
	resp, err = c.get(root.SecretID, "default", "", true, 0)
	must.NoError(t, err)
	must.Eq(t, []string{"other", "public/p"}, keysOf(resp))

	// the rules of the namespace of the request apply
	_, err = c.put(otherNS, "default", "ns", "v", nil)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	ok, err = c.put(otherNS, "other", "ns", "v", nil)
	must.NoError(t, err)
	must.True(t, ok)

	// without a token: the anonymous policy
	resp, err = c.get("", "default", "public/p", false, 0)
	must.NoError(t, err)
	must.Eq(t, []string{"public/p"}, keysOf(resp))
	_, err = c.get("", "default", "other", false, 0)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	_, err = c.put("", "default", "public/p", "w", nil)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())

	// a node secret gives no access to the KV
	node := mock.Node()
	must.NoError(t, st.UpsertNode(structs.MsgTypeTestSetup, 1100, node))
	_, err = c.get(node.SecretID, "default", "public/p", false, 0)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	_, err = c.put(node.SecretID, "default", "app/x", "w", nil)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	_, err = c.delete(node.SecretID, "default", "other", false, nil)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
}

// TestConsulKVEndpoint_Blocking checks the blocking queries, the check-and-set
// and the indexes.
func TestConsulKVEndpoint_Blocking(t *testing.T) {
	ci.Parallel(t)

	s1, cleanup := TestServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	c := &consulKVTest{t: t, srv: s1}
	c.waitBucket("")

	type result struct {
		resp *structs.ConsulKVGetResponse
		err  error
	}
	blocking := func(key string, prefix bool, index uint64) chan result {
		done := make(chan result, 1)
		go func() {
			resp, err := c.get("", "default", key, prefix, index)
			done <- result{resp, err}
		}()
		return done
	}
	wait := func(done chan result) *structs.ConsulKVGetResponse {
		select {
		case r := <-done:
			must.NoError(t, r.err)
			return r.resp
		case <-time.After(15 * time.Second):
			t.Fatal("the blocking query did not return")
			return nil
		}
	}

	// The first write of a fresh bucket raises the index of a missing key.
	resp, err := c.get("", "default", "fresh/key", false, 0)
	must.NoError(t, err)
	must.SliceEmpty(t, resp.Entries)
	missing := resp.Index
	must.Eq(t, 1, missing)
	done := blocking("fresh/key", false, missing)
	time.Sleep(300 * time.Millisecond)
	ok, err := c.put("", "default", "fresh/key", "first", nil)
	must.NoError(t, err)
	must.True(t, ok)
	resp = wait(done)
	must.Eq(t, []string{"fresh/key"}, keysOf(resp))
	must.Eq(t, "first", string(resp.Entries[0].Value))
	must.Greater(t, missing, resp.Index)

	// A blocking prefix query wakes on a put below the prefix.
	done = blocking("fresh/", true, resp.Index)
	time.Sleep(300 * time.Millisecond)
	_, err = c.put("", "default", "fresh/other", "second", nil)
	must.NoError(t, err)
	resp = wait(done)
	must.Eq(t, []string{"fresh/key", "fresh/other"}, keysOf(resp))

	// A query below the index returns at once; with no change it times out
	// with the same index.
	resp, err = c.get("", "default", "fresh/key", false, 1)
	must.NoError(t, err)
	revision := resp.Entries[0].Revision
	var short structs.ConsulKVGetResponse
	start := time.Now()
	must.NoError(t, msgpackrpc.CallWithCodec(rpcClient(t, s1), structs.ConsulKVGetRPCMethod, &structs.ConsulKVGetRequest{
		Key:          "fresh/key",
		QueryOptions: structs.QueryOptions{Region: "global", MinQueryIndex: resp.Index, MaxQueryTime: 500 * time.Millisecond},
	}, &short))
	must.Greater(t, 400*time.Millisecond, time.Since(start))
	must.Eq(t, resp.Index, short.Index)

	// check-and-set
	ok, err = c.put("", "default", "fresh/key", "stale", pointer.Of(revision+1000))
	must.NoError(t, err)
	must.False(t, ok)
	ok, err = c.put("", "default", "fresh/key", "create", pointer.Of(uint64(0)))
	must.NoError(t, err)
	must.False(t, ok)
	ok, err = c.put("", "default", "fresh/key", "v2", pointer.Of(revision))
	must.NoError(t, err)
	must.True(t, ok)
	ok, err = c.put("", "default", "fresh/new", "created", pointer.Of(uint64(0)))
	must.NoError(t, err)
	must.True(t, ok)
	resp, err = c.get("", "default", "fresh/new", false, 0)
	must.NoError(t, err)
	ok, err = c.delete("", "default", "fresh/new", false, pointer.Of(resp.Entries[0].Revision+1000))
	must.NoError(t, err)
	must.False(t, ok)
	ok, err = c.delete("", "default", "fresh/new", false, pointer.Of(resp.Entries[0].Revision))
	must.NoError(t, err)
	must.True(t, ok)

	// bad requests
	_, err = c.put("", "default", "", "v", nil)
	must.ErrorContains(t, err, "Missing key name")
	_, err = c.delete("", "default", "fresh/", true, pointer.Of(uint64(1)))
	must.ErrorContains(t, err, "Conflicting flags")
	_, err = c.put("", "default", "big", string(make([]byte, consulKVMaxValueSize+1)), nil)
	code, _, ok := structs.CodeFromRPCCodedErr(err)
	must.True(t, ok)
	must.Eq(t, http.StatusRequestEntityTooLarge, code)
}

// TestConsulKVEndpoint_NoBus: without a bus the endpoint answers 503.
func TestConsulKVEndpoint_NoBus(t *testing.T) {
	ci.Parallel(t)

	s1, cleanup := TestServer(t, nil)
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	c := &consulKVTest{t: t, srv: s1}
	_, err := c.get("", "default", "key", false, 0)
	code, _, ok := structs.CodeFromRPCCodedErr(err)
	must.True(t, ok, must.Sprint(err))
	must.Eq(t, http.StatusServiceUnavailable, code)
}

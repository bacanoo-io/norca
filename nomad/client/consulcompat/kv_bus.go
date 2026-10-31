// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consulcompat

import (
	"github.com/hashicorp/nomad/nomad/structs"
)

// busKV keeps the Consul KV in the bucket of the system bus of the servers
// (backend "bus"), through the ConsulKV RPC endpoint: the client has no
// access to the bucket, the servers apply the Variables rules of the keys
// to the token of the request, in the namespace and below the path of the
// kv block. The semantics are those of the nats backend (helper/consulkv):
// the ModifyIndex of a key is its revision, CreateIndex equals ModifyIndex
// and flags are not supported.
type busKV struct {
	backend   Backend
	namespace string
	path      string
}

func (b *busKV) Get(r *request, key string, prefix bool) ([]*kvEntry, uint64, error) {
	args := &structs.ConsulKVGetRequest{
		Key:          key,
		Prefix:       prefix,
		Path:         b.path,
		QueryOptions: r.queryOptions(b.namespace, true),
	}
	// The servers read the bucket on the leader.
	args.AllowStale = false
	var resp structs.ConsulKVGetResponse
	if err := b.backend.RPC(structs.ConsulKVGetRPCMethod, args, &resp); err != nil {
		return nil, 0, err
	}
	entries := make([]*kvEntry, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		entries = append(entries, &kvEntry{Key: e.Key, Value: e.Value, CreateIndex: e.Revision, ModifyIndex: e.Revision})
	}
	return entries, resp.Index, nil
}

func (b *busKV) Put(r *request, e *kvEntry, cas *uint64) (bool, error) {
	if e.Flags != 0 {
		return false, badRequest("flags are not supported by the bus backend of the KV")
	}
	args := &structs.ConsulKVPutRequest{
		Key:          e.Key,
		Value:        e.Value,
		CAS:          cas,
		Path:         b.path,
		WriteRequest: b.writeRequest(r),
	}
	var resp structs.ConsulKVWriteResponse
	if err := b.backend.RPC(structs.ConsulKVPutRPCMethod, args, &resp); err != nil {
		return false, err
	}
	return resp.Ok, nil
}

func (b *busKV) Delete(r *request, key string, recurse bool, cas *uint64) (bool, error) {
	args := &structs.ConsulKVDeleteRequest{
		Key:          key,
		Recurse:      recurse,
		CAS:          cas,
		Path:         b.path,
		WriteRequest: b.writeRequest(r),
	}
	var resp structs.ConsulKVWriteResponse
	if err := b.backend.RPC(structs.ConsulKVDeleteRPCMethod, args, &resp); err != nil {
		return false, err
	}
	return resp.Ok, nil
}

func (b *busKV) writeRequest(r *request) structs.WriteRequest {
	return structs.WriteRequest{Region: r.region, Namespace: b.namespace, AuthToken: r.token}
}

func (b *busKV) Close() {}

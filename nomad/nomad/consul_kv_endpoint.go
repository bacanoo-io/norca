// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/helper/consulkv"
	"github.com/hashicorp/nomad/nomad/bus"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// consulKVMaxValueSize is the largest value of a key, as in Consul.
const consulKVMaxValueSize = 512 * 1024

// ConsulKV is the RPC endpoint of the Consul KV kept in the bucket of the
// system bus (the KV backend "bus" of the Consul-compatible API of the
// clients). The clients have no access to the bucket: they forward the KV
// requests with the token of the request, and the servers apply the
// Variables rules of the path of the keys in the namespace of the request.
// The keys of every namespace and path are kept apart (ScopePrefix).
type ConsulKV struct {
	srv *Server
	ctx *RPCContext
}

func NewConsulKVEndpoint(srv *Server, ctx *RPCContext) *ConsulKV {
	return &ConsulKV{srv: srv, ctx: ctx}
}

// Get reads a key, or the keys with a prefix that the token may read. It
// blocks on the blocking index of the result like the other queries.
func (k *ConsulKV) Get(args *structs.ConsulKVGetRequest, reply *structs.ConsulKVGetResponse) error {
	authErr := k.srv.Authenticate(k.ctx, args)
	if done, err := k.srv.forward(structs.ConsulKVGetRPCMethod, args, args, reply); done {
		return err
	}
	metric := structs.RateMetricRead
	if args.Prefix {
		metric = structs.RateMetricList
	}
	k.srv.MeasureRPCRate("consul_kv", metric, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "consul_kv", "get"}, time.Now())

	if args.Key == "" && !args.Prefix {
		return consulKVBadRequest("Missing key name")
	}
	op := acl.VariablesCapabilityRead
	if args.Prefix {
		op = acl.VariablesCapabilityList
	}
	aclObj, path, err := k.authorize(args, args.RequestNamespace(), args.Path, op, args.Key)
	if err != nil {
		return err
	}
	kv, err := k.srv.consulKVHandle()
	if err != nil {
		return err
	}

	namespace := args.RequestNamespace()
	found, index, err := consulkv.Get(k.srv.shutdownCtx, kv, consulkv.ScopePrefix(namespace, path),
		args.Key, args.Prefix, args.MinQueryIndex, args.TimeToBlock())
	if err != nil {
		return consulKVError(err)
	}
	reply.Entries = make([]*structs.ConsulKVEntry, 0, len(found))
	for _, e := range found {
		// white room bugfix: only the keys the token may read are listed.
		if consulKVAllowed(aclObj, namespace, path, acl.VariablesCapabilityRead, e.Key) {
			reply.Entries = append(reply.Entries, &structs.ConsulKVEntry{Key: e.Key, Value: e.Value, Revision: e.Revision})
		}
	}
	reply.Index = index
	reply.KnownLeader = true
	return nil
}

// Put writes a key; with CAS set only when its revision is *CAS.
func (k *ConsulKV) Put(args *structs.ConsulKVPutRequest, reply *structs.ConsulKVWriteResponse) error {
	authErr := k.srv.Authenticate(k.ctx, args)
	if done, err := k.srv.forward(structs.ConsulKVPutRPCMethod, args, args, reply); done {
		return err
	}
	k.srv.MeasureRPCRate("consul_kv", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "consul_kv", "put"}, time.Now())

	if args.Key == "" {
		return consulKVBadRequest("Missing key name")
	}
	if len(args.Value) > consulKVMaxValueSize {
		return structs.NewErrRPCCoded(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("Value exceeds %d byte limit", consulKVMaxValueSize))
	}
	_, path, err := k.authorize(args, args.RequestNamespace(), args.Path, acl.VariablesCapabilityWrite, args.Key)
	if err != nil {
		return err
	}
	kv, err := k.srv.consulKVHandle()
	if err != nil {
		return err
	}
	ok, err := consulkv.Put(k.srv.shutdownCtx, kv, consulkv.ScopePrefix(args.RequestNamespace(), path),
		args.Key, args.Value, args.CAS)
	if err != nil {
		return consulKVError(err)
	}
	reply.Ok = ok
	return nil
}

// Delete deletes a key; with CAS set only when its revision is *CAS. With
// Recurse it deletes the keys with the prefix one by one (not atomic): the
// token needs list on the prefix and destroy on every key it may read.
func (k *ConsulKV) Delete(args *structs.ConsulKVDeleteRequest, reply *structs.ConsulKVWriteResponse) error {
	authErr := k.srv.Authenticate(k.ctx, args)
	if done, err := k.srv.forward(structs.ConsulKVDeleteRPCMethod, args, args, reply); done {
		return err
	}
	k.srv.MeasureRPCRate("consul_kv", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "consul_kv", "delete"}, time.Now())

	if args.Key == "" && !args.Recurse {
		return consulKVBadRequest("Missing key name")
	}
	if args.CAS != nil && args.Recurse {
		return consulKVBadRequest("Conflicting flags: cas, recurse")
	}
	op := acl.VariablesCapabilityDestroy
	if args.Recurse {
		op = acl.VariablesCapabilityList
	}
	aclObj, path, err := k.authorize(args, args.RequestNamespace(), args.Path, op, args.Key)
	if err != nil {
		return err
	}
	kv, err := k.srv.consulKVHandle()
	if err != nil {
		return err
	}
	ctx := k.srv.shutdownCtx
	namespace := args.RequestNamespace()
	scope := consulkv.ScopePrefix(namespace, path)
	if !args.Recurse {
		ok, err := consulkv.Delete(ctx, kv, scope, args.Key, args.CAS)
		if err != nil {
			return consulKVError(err)
		}
		reply.Ok = ok
		return nil
	}

	found, _, err := consulkv.Get(ctx, kv, scope, args.Key, true, 0, 0)
	if err != nil {
		return consulKVError(err)
	}
	for _, e := range found {
		if !consulKVAllowed(aclObj, namespace, path, acl.VariablesCapabilityRead, e.Key) {
			continue // not listed
		}
		// white room bugfix: every key needs destroy.
		if !consulKVAllowed(aclObj, namespace, path, acl.VariablesCapabilityDestroy, e.Key) {
			return structs.ErrPermissionDenied
		}
		if _, err := consulkv.Delete(ctx, kv, scope, e.Key, nil); err != nil {
			return consulKVError(err)
		}
	}
	reply.Ok = true
	return nil
}

// authorize resolves the ACL of the request and checks the Variables
// capability op on the path of key below the Variables path of the kv block
// (consul/kv by default), in the namespace of the request. It returns the
// ACL (nil: ACLs disabled) and the path.
//
// white room bugfix: the clients have no access to the bucket of the Consul
// KV; this is where the Variables rules of the keys are enforced, with the
// token of the KV request (the anonymous policy without one). A node secret
// has no ACL and is denied.
func (k *ConsulKV) authorize(args structs.RequestWithIdentity, namespace, path, op, key string) (*acl.ACL, string, error) {
	path = strings.Trim(path, "/")
	if path == "" {
		path = consulkv.DefaultPath
	}
	aclObj, err := k.srv.ResolveACL(args)
	if err != nil {
		return nil, "", err
	}
	if !consulKVAllowed(aclObj, namespace, path, op, key) {
		return nil, "", structs.ErrPermissionDenied
	}
	return aclObj, path, nil
}

// consulKVAllowed reports whether aclObj (nil: ACLs disabled) has the
// Variables capability op on key.
func consulKVAllowed(aclObj *acl.ACL, namespace, path, op, key string) bool {
	return aclObj == nil || aclObj.AllowVariableOperation(namespace, consulkv.ACLPath(path, key), op, nil)
}

func consulKVBadRequest(msg string) error {
	return structs.NewErrRPCCoded(http.StatusBadRequest, msg)
}

// consulKVUnavailable is the error of a bucket that cannot be used now.
func consulKVUnavailable(err error) error {
	return structs.NewErrRPCCoded(http.StatusServiceUnavailable,
		"the Consul KV of the system bus is not available: "+err.Error())
}

// consulKVError maps the errors of the bus that a retry may resolve to
// 503.
func consulKVError(err error) error {
	switch {
	case errors.Is(err, nats.ErrNoResponders), errors.Is(err, nats.ErrTimeout),
		errors.Is(err, nats.ErrConnectionClosed), errors.Is(err, nats.ErrConnectionReconnecting),
		errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled),
		errors.Is(err, jetstream.ErrNoStreamResponse), errors.Is(err, jetstream.ErrStreamNotFound),
		errors.Is(err, jetstream.ErrBucketNotFound), errors.Is(err, jetstream.ErrJetStreamNotEnabled):
		return consulKVUnavailable(err)
	}
	return err
}

// consulKVConn is the long-lived in-process connection of the ConsulKV
// endpoint to the bucket of the Consul KV. It reconnects by itself when the
// embedded NATS server restarts.
type consulKVConn struct {
	l  sync.Mutex
	nc *nats.Conn
	kv jetstream.KeyValue
}

// close closes the connection, on the shutdown of the bus.
func (c *consulKVConn) close() {
	c.l.Lock()
	defer c.l.Unlock()
	if c.nc != nil {
		c.nc.Close()
	}
	c.nc, c.kv = nil, nil
}

// consulKVHandle returns the bucket of the Consul KV for the ConsulKV
// endpoint, a coded 503 error when the server runs no bus or the bucket
// cannot be used yet.
func (s *Server) consulKVHandle() (jetstream.KeyValue, error) {
	if s.bus == nil {
		return nil, consulKVUnavailable(errors.New("the servers run no system bus"))
	}
	if !s.bus.JetStreamReady() {
		return nil, consulKVUnavailable(errors.New("the system bus has no JetStream meta leader"))
	}
	c := &s.consulKV
	c.l.Lock()
	defer c.l.Unlock()
	if c.kv != nil {
		return c.kv, nil
	}
	if c.nc == nil {
		nc, err := s.bus.Connect(nats.Name("norca-server-consul-kv"))
		if err != nil {
			return nil, consulKVUnavailable(err)
		}
		c.nc = nc
	}
	js, err := jetstream.New(c.nc)
	if err != nil {
		return nil, consulKVUnavailable(err)
	}
	ctx, cancel := context.WithTimeout(s.shutdownCtx, consulkv.Timeout)
	defer cancel()
	kv, err := js.KeyValue(ctx, bus.ConsulKVBucket)
	if err != nil {
		return nil, consulKVUnavailable(err)
	}
	c.kv = kv
	return kv, nil
}

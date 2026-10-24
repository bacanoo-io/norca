// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"net/rpc"
	"sync"
	"testing"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

const testPGSchema = `{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "required": ["version"],
      "properties": {
        "version":  {"type": "string"},
        "replicas": {"type": "integer", "default": 1},
        "storage":  {"type": "string", "default": "10GiB"},
        "backup": {
          "type": "object",
          "default": {},
          "properties": {"schedule": {"type": "string", "default": "0 2 * * *"}}
        }
      }
    },
    "status": {
      "type": "object",
      "properties": {
        "observedGeneration": {"type": "integer"},
        "readyReplicas": {"type": "integer"},
        "conditions": {"type": "array"}
      }
    }
  }
}`

func testPGKind(namespaced bool) *structs.ObjectKind {
	return &structs.ObjectKind{
		Group:      "db.example.com",
		Kind:       "PostgresCluster",
		Namespaced: namespaced,
		Schema:     testPGSchema,
		Rules: []*structs.ObjectKindRule{
			{Expression: "self.spec.replicas >= 1 && self.spec.replicas <= 7", Message: "The number of replicas must be between 1 and 7."},
			{Expression: "!has(oldSelf) || quantity(self.spec.storage) >= quantity(oldSelf.spec.storage)", Message: "Storage cannot be reduced."},
		},
		Columns: []*structs.ObjectKindColumn{{Name: "Version", Path: "spec.version"}},
	}
}

func testPGObject(ns, name string, spec map[string]any) *structs.CustomObject {
	return &structs.CustomObject{
		Group:     "db.example.com",
		Kind:      "PostgresCluster",
		Namespace: ns,
		Name:      name,
		Spec:      spec,
	}
}

type objectClient struct {
	t     *testing.T
	codec rpc.ClientCodec
}

func (c *objectClient) upsertKind(kind *structs.ObjectKind, dryRun bool, token string) (*structs.ObjectKindUpsertResponse, error) {
	req := &structs.ObjectKindUpsertRequest{
		ObjectKind:   kind,
		DryRun:       dryRun,
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: token},
	}
	var resp structs.ObjectKindUpsertResponse
	err := msgpackrpc.CallWithCodec(c.codec, "ObjectKind.UpsertObjectKind", req, &resp)
	return &resp, err
}

func (c *objectClient) apply(obj *structs.CustomObject, cas *uint64, dryRun bool, token string) (*structs.CustomObjectWriteResponse, error) {
	req := &structs.CustomObjectApplyRequest{
		Object:       obj,
		CAS:          cas,
		DryRun:       dryRun,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: obj.Namespace, AuthToken: token},
	}
	var resp structs.CustomObjectWriteResponse
	err := msgpackrpc.CallWithCodec(c.codec, "CustomObject.Apply", req, &resp)
	return &resp, err
}

func (c *objectClient) status(ns, name string, status map[string]any, cas *uint64, token string) (*structs.CustomObjectWriteResponse, error) {
	req := &structs.CustomObjectStatusRequest{
		Group:        "db.example.com",
		Kind:         "PostgresCluster",
		Name:         name,
		Status:       status,
		CAS:          cas,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: ns, AuthToken: token},
	}
	var resp structs.CustomObjectWriteResponse
	err := msgpackrpc.CallWithCodec(c.codec, "CustomObject.UpdateStatus", req, &resp)
	return &resp, err
}

func (c *objectClient) get(ns, name, token string) (*structs.CustomObject, error) {
	req := &structs.CustomObjectSpecificRequest{
		Group:        "db.example.com",
		Kind:         "PostgresCluster",
		Name:         name,
		QueryOptions: structs.QueryOptions{Region: "global", Namespace: ns, AuthToken: token},
	}
	var resp structs.SingleCustomObjectResponse
	err := msgpackrpc.CallWithCodec(c.codec, "CustomObject.GetCustomObject", req, &resp)
	return resp.Object, err
}

func (c *objectClient) list(ns string, labels map[string]string, filter, token string) ([]*structs.CustomObject, error) {
	req := &structs.CustomObjectListRequest{
		Group:        "db.example.com",
		Kind:         "PostgresCluster",
		Labels:       labels,
		QueryOptions: structs.QueryOptions{Region: "global", Namespace: ns, Filter: filter, AuthToken: token},
	}
	var resp structs.CustomObjectListResponse
	err := msgpackrpc.CallWithCodec(c.codec, "CustomObject.List", req, &resp)
	return resp.Objects, err
}

func (c *objectClient) delete(ns, name string, cas *uint64, token string) error {
	req := &structs.CustomObjectDeleteRequest{
		Group:        "db.example.com",
		Kind:         "PostgresCluster",
		Name:         name,
		CAS:          cas,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: ns, AuthToken: token},
	}
	var resp structs.CustomObjectWriteResponse
	return msgpackrpc.CallWithCodec(c.codec, "CustomObject.Delete", req, &resp)
}

func errCode(t *testing.T, err error) int {
	t.Helper()
	must.Error(t, err)
	code, _, ok := structs.CodeFromRPCCodedErr(err)
	must.True(t, ok, must.Sprintf("not a coded error: %v", err))
	return code
}

func TestObjectKindEndpoint_CRUD(t *testing.T) {
	ci.Parallel(t)

	s, cleanupS := TestServer(t, nil)
	defer cleanupS()
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	testutil.WaitForLeader(t, s.RPC)

	// A dry run validates without storing.
	resp, err := c.upsertKind(testPGKind(true), true, "")
	must.NoError(t, err)
	must.Eq(t, "PostgresCluster", resp.ObjectKind.Kind)
	k, err := s.State().ObjectKindByName(nil, "db.example.com", "PostgresCluster")
	must.NoError(t, err)
	must.Nil(t, k)

	resp, err = c.upsertKind(testPGKind(true), false, "")
	must.NoError(t, err)
	must.NonZero(t, resp.Index)
	must.Eq(t, resp.Index, resp.ObjectKind.CreateIndex)
	_, err = c.upsertKind(&structs.ObjectKind{Group: "lb.example.com", Kind: "LoadBalancer", Schema: `{}`}, false, "")
	must.NoError(t, err)

	listReq := &structs.ObjectKindListRequest{QueryOptions: structs.QueryOptions{Region: "global"}}
	var listResp structs.ObjectKindListResponse
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "ObjectKind.List", listReq, &listResp))
	must.Len(t, 2, listResp.ObjectKinds)
	listReq.Prefix = "lb."
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "ObjectKind.List", listReq, &listResp))
	must.Len(t, 1, listResp.ObjectKinds)

	getReq := &structs.ObjectKindSpecificRequest{Group: "db.example.com", Kind: "PostgresCluster", QueryOptions: structs.QueryOptions{Region: "global"}}
	var getResp structs.SingleObjectKindResponse
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "ObjectKind.GetObjectKind", getReq, &getResp))
	must.NotNil(t, getResp.ObjectKind)
	must.Len(t, 2, getResp.ObjectKind.Rules)

	// Update keeps the create index.
	updated := testPGKind(true)
	updated.Description = "PostgreSQL clusters"
	resp2, err := c.upsertKind(updated, false, "")
	must.NoError(t, err)
	must.Eq(t, resp.ObjectKind.CreateIndex, resp2.ObjectKind.CreateIndex)

	// A kind with objects can't be deleted or change its scope.
	_, err = c.apply(testPGObject("default", "billing", map[string]any{"version": "16"}), nil, false, "")
	must.NoError(t, err)
	_, err = c.upsertKind(testPGKind(false), false, "")
	must.Eq(t, 409, errCode(t, err))

	delReq := &structs.ObjectKindDeleteRequest{Group: "db.example.com", Kind: "PostgresCluster", WriteRequest: structs.WriteRequest{Region: "global"}}
	var delResp structs.GenericResponse
	err = msgpackrpc.CallWithCodec(c.codec, "ObjectKind.DeleteObjectKind", delReq, &delResp)
	must.Eq(t, 409, errCode(t, err))

	must.NoError(t, c.delete("default", "billing", nil, ""))
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "ObjectKind.DeleteObjectKind", delReq, &delResp))
	err = msgpackrpc.CallWithCodec(c.codec, "ObjectKind.DeleteObjectKind", delReq, &delResp)
	must.Eq(t, 404, errCode(t, err))
}

func TestObjectKindEndpoint_Invalid(t *testing.T) {
	ci.Parallel(t)

	s, cleanupS := TestServer(t, nil)
	defer cleanupS()
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	testutil.WaitForLeader(t, s.RPC)

	mod := func(f func(k *structs.ObjectKind)) *structs.ObjectKind {
		k := testPGKind(true)
		f(k)
		return k
	}
	cases := map[string]*structs.ObjectKind{
		"bad group":       mod(func(k *structs.ObjectKind) { k.Group = "DB_example" }),
		"bad kind":        mod(func(k *structs.ObjectKind) { k.Kind = "postgres-cluster" }),
		"no schema":       mod(func(k *structs.ObjectKind) { k.Schema = "" }),
		"bad schema":      mod(func(k *structs.ObjectKind) { k.Schema = `{"type": "objekt"}` }),
		"remote ref":      mod(func(k *structs.ObjectKind) { k.Schema = `{"$ref": "https://example.com/s.json"}` }),
		"bad default":     mod(func(k *structs.ObjectKind) { k.Schema = `{"properties": {"spec": {"default": 1, "type": "object"}}}` }),
		"rule syntax":     mod(func(k *structs.ObjectKind) { k.Rules[0].Expression = "self.spec.replicas >=" }),
		"rule not bool":   mod(func(k *structs.ObjectKind) { k.Rules[0].Expression = "'x'" }),
		"bad column":      mod(func(k *structs.ObjectKind) { k.Columns[0].Path = "" }),
		"bad scale":       mod(func(k *structs.ObjectKind) { k.Scale = &structs.ObjectKindScale{SpecPath: "status.replicas"} }),
		"replicated":      mod(func(k *structs.ObjectKind) { k.Replicated = true }),
		"duplicate colum": mod(func(k *structs.ObjectKind) { k.Columns = append(k.Columns, k.Columns[0]) }),
	}
	for name, kind := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := c.upsertKind(kind, false, "")
			must.Eq(t, 400, errCode(t, err))
		})
	}

	k, err := s.State().ObjectKindByName(nil, "db.example.com", "PostgresCluster")
	must.NoError(t, err)
	must.Nil(t, k)
}

func TestCustomObjectEndpoint_Apply(t *testing.T) {
	ci.Parallel(t)

	s, cleanupS := TestServer(t, nil)
	defer cleanupS()
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	testutil.WaitForLeader(t, s.RPC)

	// The kind must exist.
	_, err := c.apply(testPGObject("default", "billing", map[string]any{"version": "16"}), nil, false, "")
	must.Eq(t, 404, errCode(t, err))

	_, err = c.upsertKind(testPGKind(true), false, "")
	must.NoError(t, err)

	// A dry run fills in the defaults and validates without storing.
	resp, err := c.apply(testPGObject("default", "billing", map[string]any{"version": "16", "replicas": 3}), nil, true, "")
	must.NoError(t, err)
	must.Eq[any](t, "10GiB", resp.Object.Spec["storage"])
	obj, err := c.get("default", "billing", "")
	must.NoError(t, err)
	must.Nil(t, obj)

	// Create: defaults are filled in and the generation is 1.
	resp, err = c.apply(testPGObject("default", "billing", map[string]any{"version": "16", "replicas": 3}), nil, false, "")
	must.NoError(t, err)
	created := resp.Object
	must.Eq(t, uint64(1), created.Generation)
	must.NotEq(t, "", created.ID)
	must.Eq(t, map[string]any{
		"version": "16", "replicas": float64(3), "storage": "10GiB",
		"backup": map[string]any{"schedule": "0 2 * * *"},
	}, created.Spec)

	obj, err = c.get("default", "billing", "")
	must.NoError(t, err)
	must.Eq(t, created.Spec, obj.Spec)
	must.Eq(t, resp.Index, obj.ModifyIndex)

	// Applying the same spec changes nothing.
	resp, err = c.apply(testPGObject("default", "billing", map[string]any{"version": "16", "replicas": 3}), nil, false, "")
	must.NoError(t, err)
	must.True(t, resp.Unchanged)
	must.Eq(t, obj.ModifyIndex, resp.Object.ModifyIndex)

	// A label change is written, but the generation stays.
	labeled := testPGObject("default", "billing", map[string]any{"version": "16", "replicas": 3})
	labeled.Labels = map[string]string{"team": "finance"}
	resp, err = c.apply(labeled, nil, false, "")
	must.NoError(t, err)
	must.False(t, resp.Unchanged)
	must.Eq(t, uint64(1), resp.Object.Generation)

	// A status write doesn't change the generation and is kept by spec
	// writes.
	sresp, err := c.status("default", "billing", map[string]any{
		"observedGeneration": 1, "readyReplicas": 3,
		"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
	}, nil, "")
	must.NoError(t, err)
	must.Eq(t, uint64(1), sresp.Object.Generation)
	must.Eq(t, uint64(1), sresp.Object.ObservedGeneration())
	must.Eq(t, map[string]string{"team": "finance"}, sresp.Object.Labels)

	// A spec change increases the generation and keeps the status.
	spec := testPGObject("default", "billing", map[string]any{"version": "16", "replicas": 5})
	spec.Labels = map[string]string{"team": "finance"}
	resp, err = c.apply(spec, nil, false, "")
	must.NoError(t, err)
	must.Eq(t, uint64(2), resp.Object.Generation)
	must.Eq[any](t, float64(3), resp.Object.Status["readyReplicas"])
	must.Eq(t, created.ID, resp.Object.ID)
	must.Eq(t, created.CreateIndex, resp.Object.CreateIndex)

	// Invalid status: observed generation ahead of the generation, or a
	// schema violation.
	_, err = c.status("default", "billing", map[string]any{"observedGeneration": 3}, nil, "")
	must.Eq(t, 400, errCode(t, err))
	_, err = c.status("default", "billing", map[string]any{"readyReplicas": "three"}, nil, "")
	must.Eq(t, 400, errCode(t, err))
	_, err = c.status("default", "missing", map[string]any{}, nil, "")
	must.Eq(t, 404, errCode(t, err))

	// Schema and CEL validation.
	cases := map[string]map[string]any{
		"missing version": {"replicas": 3},
		"wrong type":      {"version": 16},
		"too many":        {"version": "16", "replicas": 8},
	}
	for name, spec := range cases {
		_, err := c.apply(testPGObject("default", "billing", spec), nil, false, "")
		must.Eq(t, 400, errCode(t, err), must.Sprint(name))
	}

	// The oldSelf rule allows increasing the storage, but not reducing it.
	_, err = c.apply(testPGObject("default", "billing", map[string]any{"version": "16", "replicas": 5, "storage": "20GiB"}), nil, false, "")
	must.NoError(t, err)
	_, err = c.apply(testPGObject("default", "billing", map[string]any{"version": "16", "replicas": 5, "storage": "5GiB"}), nil, false, "")
	must.Eq(t, 400, errCode(t, err))
	must.ErrorContains(t, err, "Storage cannot be reduced.")

	// A dry run of an update shows the new generation without storing it.
	before, err := c.get("default", "billing", "")
	must.NoError(t, err)
	resp, err = c.apply(testPGObject("default", "billing", map[string]any{"version": "17", "replicas": 5, "storage": "20GiB"}), nil, true, "")
	must.NoError(t, err)
	must.Eq(t, before.Generation+1, resp.Object.Generation)
	after, err := c.get("default", "billing", "")
	must.NoError(t, err)
	must.Eq(t, before, after)

	// Unknown namespace, finalizers and unsupported propagation are
	// rejected.
	_, err = c.apply(testPGObject("nope", "billing", map[string]any{"version": "16"}), nil, false, "")
	must.Eq(t, 400, errCode(t, err))
	fin := testPGObject("default", "other", map[string]any{"version": "16"})
	fin.Finalizers = []string{"example.com/cleanup"}
	_, err = c.apply(fin, nil, false, "")
	must.Eq(t, 400, errCode(t, err))
	delReq := &structs.CustomObjectDeleteRequest{
		Group: "db.example.com", Kind: "PostgresCluster", Name: "billing", Propagation: "orphan",
		WriteRequest: structs.WriteRequest{Region: "global"},
	}
	var delResp structs.CustomObjectWriteResponse
	err = msgpackrpc.CallWithCodec(c.codec, "CustomObject.Delete", delReq, &delResp)
	must.Eq(t, 400, errCode(t, err))
}

func TestCustomObjectEndpoint_Concurrency(t *testing.T) {
	ci.Parallel(t)

	s, cleanupS := TestServer(t, nil)
	defer cleanupS()
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	testutil.WaitForLeader(t, s.RPC)

	_, err := c.upsertKind(testPGKind(true), false, "")
	must.NoError(t, err)

	// Create with cas=0 succeeds once.
	resp, err := c.apply(testPGObject("default", "billing", map[string]any{"version": "16"}), pointer.Of(uint64(0)), false, "")
	must.NoError(t, err)
	_, err = c.apply(testPGObject("default", "billing", map[string]any{"version": "16"}), pointer.Of(uint64(0)), false, "")
	must.True(t, structs.IsObjectConflictError(err))

	// Two writers read the same version: the second gets a conflict and
	// succeeds after re-reading.
	read := resp.Object.ModifyIndex
	_, err = c.apply(testPGObject("default", "billing", map[string]any{"version": "16", "replicas": 2}), &read, false, "")
	must.NoError(t, err)
	_, err = c.apply(testPGObject("default", "billing", map[string]any{"version": "16", "replicas": 3}), &read, false, "")
	must.Eq(t, 409, errCode(t, err))
	must.True(t, structs.IsObjectConflictError(err))
	current, err := c.get("default", "billing", "")
	must.NoError(t, err)
	_, err = c.apply(testPGObject("default", "billing", map[string]any{"version": "16", "replicas": 3}), &current.ModifyIndex, false, "")
	must.NoError(t, err)

	// Status writes use check-and-set too.
	_, err = c.status("default", "billing", map[string]any{"readyReplicas": 1}, &read, "")
	must.True(t, structs.IsObjectConflictError(err))
	err = c.delete("default", "billing", &read, "")
	must.True(t, structs.IsObjectConflictError(err))

	// Writes without an index are retried on conflicts, so concurrent
	// writers all succeed.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codec := rpcClient(t, s)
			cc := &objectClient{t: t, codec: codec}
			_, err := cc.status("default", "billing", map[string]any{"readyReplicas": i}, nil, "")
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		must.NoError(t, err)
	}
	final, err := c.get("default", "billing", "")
	must.NoError(t, err)
	must.Eq(t, uint64(3), final.Generation)
	must.NotNil(t, final.Status["readyReplicas"])
}

func TestCustomObjectEndpoint_ListAndScope(t *testing.T) {
	ci.Parallel(t)

	s, cleanupS := TestServer(t, nil)
	defer cleanupS()
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	testutil.WaitForLeader(t, s.RPC)

	ns := mock.Namespace()
	ns.Name = "prod"
	must.NoError(t, s.State().UpsertNamespaces(100, []*structs.Namespace{ns}))
	_, err := c.upsertKind(testPGKind(true), false, "")
	must.NoError(t, err)

	for i, target := range []struct{ ns, name, team string }{
		{"default", "a", "finance"}, {"default", "b", "web"}, {"prod", "a", "finance"}, {"prod", "c", ""},
	} {
		obj := testPGObject(target.ns, target.name, map[string]any{"version": "16", "replicas": i + 1})
		if target.team != "" {
			obj.Labels = map[string]string{"team": target.team}
		}
		_, err := c.apply(obj, nil, false, "")
		must.NoError(t, err)
	}

	names := func(objs []*structs.CustomObject) []string {
		var out []string
		for _, o := range objs {
			out = append(out, o.Namespace+"/"+o.Name)
		}
		return out
	}

	objs, err := c.list("default", nil, "", "")
	must.NoError(t, err)
	must.Eq(t, []string{"default/a", "default/b"}, names(objs))
	objs, err = c.list("*", nil, "", "")
	must.NoError(t, err)
	must.Eq(t, []string{"default/a", "default/b", "prod/a", "prod/c"}, names(objs))
	objs, err = c.list("*", map[string]string{"team": "finance"}, "", "")
	must.NoError(t, err)
	must.Eq(t, []string{"default/a", "prod/a"}, names(objs))
	objs, err = c.list("prod", map[string]string{"team": "finance"}, "", "")
	must.NoError(t, err)
	must.Eq(t, []string{"prod/a"}, names(objs))
	objs, err = c.list("*", nil, `Labels.team == "web" or Name == "c"`, "")
	must.NoError(t, err)
	must.Eq(t, []string{"default/b", "prod/c"}, names(objs))
	objs, err = c.list("*", nil, `Spec.version == "16" and Namespace == "prod"`, "")
	must.NoError(t, err)
	must.Eq(t, []string{"prod/a", "prod/c"}, names(objs))

	// Pagination.
	req := &structs.CustomObjectListRequest{
		Group: "db.example.com", Kind: "PostgresCluster",
		QueryOptions: structs.QueryOptions{Region: "global", Namespace: "*", PerPage: 3},
	}
	var page structs.CustomObjectListResponse
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "CustomObject.List", req, &page))
	must.Eq(t, []string{"default/a", "default/b", "prod/a"}, names(page.Objects))
	must.Eq(t, "prod,c", page.NextToken)
	req.NextToken = page.NextToken
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "CustomObject.List", req, &page))
	must.Eq(t, []string{"prod/c"}, names(page.Objects))

	// Objects of a cluster-scoped kind have no namespace.
	_, err = c.upsertKind(&structs.ObjectKind{Group: "lb.example.com", Kind: "LoadBalancer", Schema: `{}`}, false, "")
	must.NoError(t, err)
	lb := &structs.CustomObject{Group: "lb.example.com", Kind: "LoadBalancer", Namespace: "prod", Name: "edge", Spec: map[string]any{"port": 443}}
	resp, err := c.apply(lb, nil, false, "")
	must.NoError(t, err)
	must.Eq(t, "", resp.Object.Namespace)
	stored, err := s.State().CustomObjectByName(nil, "lb.example.com", "LoadBalancer", "", "edge")
	must.NoError(t, err)
	must.NotNil(t, stored)
}

func TestCustomObjectEndpoint_ACL(t *testing.T) {
	ci.Parallel(t)

	s, root, cleanupS := TestACLServer(t, nil)
	defer cleanupS()
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	testutil.WaitForLeader(t, s.RPC)

	ns := mock.Namespace()
	ns.Name = "prod"
	must.NoError(t, s.State().UpsertNamespaces(900, []*structs.Namespace{ns}))

	operator := mock.CreatePolicyAndToken(t, s.State(), 1001, "operator", `operator { policy = "write" }`)
	user := mock.CreatePolicyAndToken(t, s.State(), 1002, "user", `
namespace "default" {
  object "db.example.com/PostgresCluster" { capabilities = ["read", "write"] }
}`)
	controller := mock.CreatePolicyAndToken(t, s.State(), 1003, "controller", `
namespace "*" {
  object "db.example.com/*" { capabilities = ["read", "write-status"] }
}`)
	other := mock.CreatePolicyAndToken(t, s.State(), 1004, "other", `namespace "default" { policy = "write" }`)

	// Kinds: operator write.
	for _, token := range []string{"", user.SecretID, controller.SecretID, other.SecretID} {
		_, err := c.upsertKind(testPGKind(true), false, token)
		must.EqError(t, err, structs.ErrPermissionDenied.Error())
	}
	_, err := c.upsertKind(testPGKind(true), false, operator.SecretID)
	must.NoError(t, err)

	// Reading kinds: operator read or an object capability on the kind.
	getKind := func(token string) error {
		req := &structs.ObjectKindSpecificRequest{Group: "db.example.com", Kind: "PostgresCluster",
			QueryOptions: structs.QueryOptions{Region: "global", AuthToken: token}}
		var resp structs.SingleObjectKindResponse
		return msgpackrpc.CallWithCodec(c.codec, "ObjectKind.GetObjectKind", req, &resp)
	}
	for _, token := range []string{operator.SecretID, user.SecretID, controller.SecretID} {
		must.NoError(t, getKind(token))
	}
	must.EqError(t, getKind(other.SecretID), structs.ErrPermissionDenied.Error())
	listReq := &structs.ObjectKindListRequest{QueryOptions: structs.QueryOptions{Region: "global", AuthToken: other.SecretID}}
	var listResp structs.ObjectKindListResponse
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "ObjectKind.List", listReq, &listResp))
	must.Len(t, 0, listResp.ObjectKinds)

	// Spec writes: write capability; status writes: write-status.
	_, err = c.apply(testPGObject("default", "billing", map[string]any{"version": "16"}), nil, false, controller.SecretID)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())
	_, err = c.apply(testPGObject("default", "billing", map[string]any{"version": "16"}), nil, false, user.SecretID)
	must.NoError(t, err)
	_, err = c.apply(testPGObject("prod", "billing", map[string]any{"version": "16"}), nil, false, user.SecretID)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())
	_, err = c.apply(testPGObject("prod", "billing", map[string]any{"version": "16"}), nil, false, root.SecretID)
	must.NoError(t, err)

	_, err = c.status("default", "billing", map[string]any{"readyReplicas": 1}, nil, user.SecretID)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())
	_, err = c.status("default", "billing", map[string]any{"readyReplicas": 1}, nil, controller.SecretID)
	must.NoError(t, err)

	// Reads.
	_, err = c.get("default", "billing", other.SecretID)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())
	_, err = c.get("prod", "billing", user.SecretID)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())
	obj, err := c.get("prod", "billing", controller.SecretID)
	must.NoError(t, err)
	must.NotNil(t, obj)

	// Listing all namespaces is filtered per namespace.
	objs, err := c.list("*", nil, "", user.SecretID)
	must.NoError(t, err)
	must.Len(t, 1, objs)
	objs, err = c.list("*", nil, "", controller.SecretID)
	must.NoError(t, err)
	must.Len(t, 2, objs)
	_, err = c.list("*", nil, "", other.SecretID)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())

	// Deletes: write capability.
	must.EqError(t, c.delete("default", "billing", nil, controller.SecretID), structs.ErrPermissionDenied.Error())
	must.NoError(t, c.delete("default", "billing", nil, user.SecretID))

	// A workload identity gets the capabilities of the policies bound to
	// its job.
	job := mock.Job()
	alloc := mock.Alloc()
	alloc.Job = job
	alloc.JobID = job.ID
	alloc.ClientStatus = structs.AllocClientStatusRunning
	must.NoError(t, s.State().UpsertJob(structs.MsgTypeTestSetup, 1100, nil, job))
	must.NoError(t, s.State().UpsertAllocs(structs.MsgTypeTestSetup, 1101, []*structs.Allocation{alloc}))
	policy := &structs.ACLPolicy{
		Name:   "pg-operator",
		Rules:  `namespace "*" { object "db.example.com/*" { capabilities = ["read", "write-status"] } }`,
		JobACL: &structs.JobACL{Namespace: job.Namespace, JobID: job.ID},
	}
	policy.SetHash()
	must.NoError(t, s.State().UpsertACLPolicies(structs.MsgTypeTestSetup, 1102, []*structs.ACLPolicy{policy}))
	claims := alloc.ToTaskIdentityClaims(job, job.TaskGroups[0].Tasks[0].Name)
	// Wait for the root key: SignClaims returns an empty token if the key
	// becomes active while it waits for it.
	testutil.WaitForResult(func() (bool, error) {
		_, err := s.encrypter.activeKeySet()
		return err == nil, err
	}, func(err error) { t.Fatalf("keyring not ready: %v", err) })
	wiToken, _, err := s.encrypter.SignClaims(claims)
	must.NoError(t, err)
	must.NotEq(t, "", wiToken)

	obj, err = c.get("prod", "billing", wiToken)
	must.NoError(t, err)
	must.NotNil(t, obj)
	_, err = c.status("prod", "billing", map[string]any{"readyReplicas": 2}, nil, wiToken)
	must.NoError(t, err)
	_, err = c.apply(testPGObject("prod", "billing", map[string]any{"version": "17"}), nil, false, wiToken)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())
	must.EqError(t, fmt.Errorf("%v", c.delete("prod", "billing", nil, wiToken)), structs.ErrPermissionDenied.Error())
}

func TestCustomObjectEndpoint_ManagedByRule(t *testing.T) {
	ci.Parallel(t)

	s, root, cleanupS := TestACLServer(t, nil)
	defer cleanupS()
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	testutil.WaitForLeader(t, s.RPC)

	gitops := mock.CreatePolicyAndToken(t, s.State(), 1001, "gitops", `
namespace "default" { object "db.example.com/*" { policy = "write" } }`)
	gitops.Name = "gitops"

	// Only the writer named in managedBy may change a managed object.
	kind := testPGKind(true)
	kind.Rules = append(kind.Rules, &structs.ObjectKindRule{
		Expression: "!has(oldSelf) || oldSelf.metadata.managedBy == '' || request.user.policies.exists(p, p == oldSelf.metadata.managedBy)",
		Message:    "The object is managed by another writer.",
	})
	_, err := c.upsertKind(kind, false, root.SecretID)
	must.NoError(t, err)

	obj := testPGObject("default", "billing", map[string]any{"version": "16"})
	obj.ManagedBy = "gitops"
	_, err = c.apply(obj, nil, false, gitops.SecretID)
	must.NoError(t, err)

	obj.Spec = map[string]any{"version": "17"}
	_, err = c.apply(obj, nil, false, root.SecretID)
	must.ErrorContains(t, err, "managed by another writer")
	_, err = c.apply(obj, nil, false, gitops.SecretID)
	must.NoError(t, err)
}

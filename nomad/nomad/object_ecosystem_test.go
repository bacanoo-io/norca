// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/admission"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

// testWebhookServer is a TLS admission webhook for object reviews. The
// handler decides per review; reviews are recorded.
type testWebhookServer struct {
	*httptest.Server
	mu      sync.Mutex
	reviews []*admission.ReviewRequest
	handle  func(path string, req *admission.ReviewRequest) *admission.ReviewResponse
}

func newTestWebhookServer(t *testing.T, handle func(path string, req *admission.ReviewRequest) *admission.ReviewResponse) *testWebhookServer {
	ws := &testWebhookServer{handle: handle}
	ws.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var review admission.Review
		if err := json.Unmarshal(body, &review); err != nil || review.Request == nil {
			http.Error(w, "bad review", http.StatusBadRequest)
			return
		}
		ws.mu.Lock()
		ws.reviews = append(ws.reviews, review.Request)
		ws.mu.Unlock()
		resp := ws.handle(r.URL.Path, review.Request)
		resp.UID = review.Request.UID
		_ = json.NewEncoder(w).Encode(&admission.Review{APIVersion: admission.ReviewAPIVersion, Kind: admission.ReviewKind, Response: resp})
	}))
	t.Cleanup(ws.Close)
	return ws
}

// caPEM returns the PEM of the server's certificate, its own CA.
func (ws *testWebhookServer) caPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ws.Certificate().Raw}))
}

func (ws *testWebhookServer) count() int {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return len(ws.reviews)
}

func (ws *testWebhookServer) last() *admission.ReviewRequest {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.reviews[len(ws.reviews)-1]
}

func TestObjectKindWebhooks(t *testing.T) {
	ci.Parallel(t)

	s, cleanupS := TestServer(t, nil)
	defer cleanupS()
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	testutil.WaitForLeader(t, s.RPC)

	ws := newTestWebhookServer(t, func(path string, req *admission.ReviewRequest) *admission.ReviewResponse {
		spec, _ := req.Object["spec"].(map[string]any)
		switch path {
		case "/default":
			if _, ok := spec["storage"].(string); ok && spec["storage"] != "10GiB" {
				return &admission.ReviewResponse{Allowed: true}
			}
			// Raise the default storage of the schema.
			patch, _ := json.Marshal([]map[string]any{{"op": "add", "path": "/spec/storage", "value": "20GiB"}})
			return &admission.ReviewResponse{Allowed: true, Patch: patch, Warnings: []string{"storage defaulted"}}
		case "/validate":
			if spec["version"] == "9" {
				return &admission.ReviewResponse{Allowed: false, Message: "version 9 is not supported"}
			}
			return &admission.ReviewResponse{Allowed: true}
		case "/patch-metadata":
			patch, _ := json.Marshal([]map[string]any{{"op": "replace", "path": "/metadata/name", "value": "other"}})
			return &admission.ReviewResponse{Allowed: true, Patch: patch}
		}
		return &admission.ReviewResponse{Allowed: false, Message: "unknown path"}
	})

	kind := testPGKind(true)
	kind.Webhooks = []*structs.ObjectKindWebhook{
		{Name: "default", Mode: structs.ObjectWebhookModeDefault, URL: ws.URL + "/default", CACert: ws.caPEM()},
		{Name: "validate", URL: ws.URL + "/validate", CACert: ws.caPEM(),
			Operations: []string{structs.ObjectWebhookOperationCreate, structs.ObjectWebhookOperationUpdate, structs.ObjectWebhookOperationDelete}},
	}
	// Approvals in the request are ignored; without ACLs the writer is
	// management and approves the webhooks.
	kind.Webhooks[0].Approval = &structs.ObjectKindWebhookApproval{Digest: "forged"}
	resp, err := c.upsertKind(kind, false, "")
	must.NoError(t, err)
	stored, err := s.State().ObjectKindByName(nil, kind.Group, kind.Kind)
	must.NoError(t, err)
	must.Len(t, 2, stored.Webhooks)
	for _, w := range stored.Webhooks {
		must.True(t, w.Approved(), must.Sprintf("webhook %s", w.Name))
		must.Eq(t, structs.DefaultObjectWebhookTimeout, w.Timeout)
		must.Eq(t, structs.ObjectWebhookFailurePolicyFail, w.FailurePolicy)
	}
	must.SliceEmpty(t, resp.Warnings)

	// The defaulting webhook sets the storage, the validating one sees the
	// result.
	out, err := c.apply(testPGObject("default", "billing", map[string]any{"version": "16"}), nil, false, "")
	must.NoError(t, err)
	must.Eq(t, "20GiB", out.Object.Spec["storage"])
	must.Eq(t, []string{`webhook "default": storage defaulted`}, out.Warnings)
	must.Eq(t, 2, ws.count())
	last := ws.last()
	must.Eq(t, structs.AdmissionResourceObject, last.Resource)
	must.Eq(t, "db.example.com/PostgresCluster", last.ObjectKind)
	must.Eq(t, "create", last.Operation)
	lastSpec := last.Object["spec"].(map[string]any)
	must.Eq(t, "20GiB", lastSpec["storage"])

	// A user-given storage is kept.
	out, err = c.apply(testPGObject("default", "billing", map[string]any{"version": "16", "storage": "50GiB"}), nil, false, "")
	must.NoError(t, err)
	must.Eq(t, "50GiB", out.Object.Spec["storage"])
	must.Eq(t, uint64(2), out.Object.Generation)
	must.Eq(t, "update", ws.last().Operation)
	must.NotNil(t, ws.last().OldObject)

	// The validating webhook denies.
	_, err = c.apply(testPGObject("default", "other", map[string]any{"version": "9"}), nil, false, "")
	must.ErrorContains(t, err, "version 9 is not supported")
	must.Eq(t, 400, errCode(t, err))
	got, err := c.get("default", "other", "")
	must.NoError(t, err)
	must.Nil(t, got)

	// Status writes are not reviewed.
	n := ws.count()
	_, err = c.status("default", "billing", map[string]any{"observedGeneration": 2}, nil, "")
	must.NoError(t, err)
	must.Eq(t, n, ws.count())

	// Deletions are reviewed by the webhook that asked for them.
	must.NoError(t, c.delete("default", "billing", nil, ""))
	must.Eq(t, "delete", ws.last().Operation)

	// A patch outside the spec is a webhook failure (fail closed).
	kind = testPGKind(true)
	kind.Webhooks = []*structs.ObjectKindWebhook{
		{Name: "bad", Mode: structs.ObjectWebhookModeDefault, URL: ws.URL + "/patch-metadata", CACert: ws.caPEM()},
	}
	_, err = c.upsertKind(kind, false, "")
	must.NoError(t, err)
	_, err = c.apply(testPGObject("default", "x", map[string]any{"version": "16"}), nil, false, "")
	must.ErrorContains(t, err, "patch may only change the spec")

	// An unreachable webhook fails closed, unless its failure policy is
	// "ignore".
	kind.Webhooks = []*structs.ObjectKindWebhook{{Name: "down", URL: "https://127.0.0.1:1/validate", Timeout: time.Second}}
	_, err = c.upsertKind(kind, false, "")
	must.NoError(t, err)
	_, err = c.apply(testPGObject("default", "x", map[string]any{"version": "16"}), nil, false, "")
	must.ErrorContains(t, err, `webhook "down" of db.example.com/PostgresCluster failed`)
	must.Eq(t, 403, errCode(t, err))

	kind.Webhooks[0].FailurePolicy = structs.ObjectWebhookFailurePolicyIgnore
	_, err = c.upsertKind(kind, false, "")
	must.NoError(t, err)
	out, err = c.apply(testPGObject("default", "x", map[string]any{"version": "16"}), nil, false, "")
	must.NoError(t, err)
	must.Len(t, 1, out.Warnings)
	must.StrContains(t, out.Warnings[0], "failed and was ignored")
}

func TestObjectKindWebhooks_Approval(t *testing.T) {
	ci.Parallel(t)

	s, root, cleanupS := TestACLServer(t, nil)
	defer cleanupS()
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	testutil.WaitForLeader(t, s.RPC)

	ws := newTestWebhookServer(t, func(string, *admission.ReviewRequest) *admission.ReviewResponse {
		return &admission.ReviewResponse{Allowed: true}
	})
	operator := mock.CreatePolicyAndToken(t, s.State(), 1001, "operator", `
operator { policy = "write" }
namespace "default" {
  object "db.example.com/*" { capabilities = ["read", "write"] }
}`)

	kind := testPGKind(true)
	kind.Webhooks = []*structs.ObjectKindWebhook{{Name: "validate", URL: ws.URL + "/validate", CACert: ws.caPEM()}}

	// A kind written by an operator token leaves the webhook pending, and
	// writes fail closed until it is approved.
	resp, err := c.upsertKind(kind, false, operator.SecretID)
	must.NoError(t, err)
	must.Len(t, 1, resp.Warnings)
	must.StrContains(t, resp.Warnings[0], "pending approval")
	_, err = c.apply(testPGObject("default", "billing", map[string]any{"version": "16"}), nil, false, operator.SecretID)
	must.ErrorContains(t, err, "not approved")
	must.Eq(t, 0, ws.count())

	approve := func(token string, revoke bool) (*structs.ObjectKindUpsertResponse, error) {
		req := &structs.ObjectKindWebhookApprovalRequest{
			Group: kind.Group, Kind: kind.Kind, Webhook: "validate", Revoke: revoke,
			WriteRequest: structs.WriteRequest{Region: "global", AuthToken: token},
		}
		var out structs.ObjectKindUpsertResponse
		err := msgpackrpc.CallWithCodec(c.codec, "ObjectKind.ApproveWebhook", req, &out)
		return &out, err
	}

	// Only management tokens approve.
	_, err = approve(operator.SecretID, false)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	out, err := approve(root.SecretID, false)
	must.NoError(t, err)
	w := out.ObjectKind.Webhook("validate")
	must.True(t, w.Approved())
	must.StrContains(t, w.Approval.ApprovedBy, root.AccessorID)

	_, err = c.apply(testPGObject("default", "billing", map[string]any{"version": "16"}), nil, false, operator.SecretID)
	must.NoError(t, err)
	must.Eq(t, 1, ws.count())

	// Writing the same kind again keeps the approval; a changed webhook
	// needs a new one.
	_, err = c.upsertKind(kind, false, operator.SecretID)
	must.NoError(t, err)
	stored, _ := s.State().ObjectKindByName(nil, kind.Group, kind.Kind)
	must.True(t, stored.Webhooks[0].Approved())

	kind.Webhooks[0].URL = ws.URL + "/elsewhere"
	_, err = c.upsertKind(kind, false, operator.SecretID)
	must.NoError(t, err)
	stored, _ = s.State().ObjectKindByName(nil, kind.Group, kind.Kind)
	must.False(t, stored.Webhooks[0].Approved())

	// Revoking an approval.
	_, err = approve(root.SecretID, false)
	must.NoError(t, err)
	_, err = approve(root.SecretID, true)
	must.NoError(t, err)
	stored, _ = s.State().ObjectKindByName(nil, kind.Group, kind.Kind)
	must.Nil(t, stored.Webhooks[0].Approval)
}

func TestObjectKindEndpoint_CAS(t *testing.T) {
	ci.Parallel(t)

	s, cleanupS := TestServer(t, nil)
	defer cleanupS()
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	testutil.WaitForLeader(t, s.RPC)

	upsert := func(cas uint64) (*structs.ObjectKindUpsertResponse, error) {
		req := &structs.ObjectKindUpsertRequest{ObjectKind: testPGKind(true), CAS: &cas, WriteRequest: structs.WriteRequest{Region: "global"}}
		var resp structs.ObjectKindUpsertResponse
		err := msgpackrpc.CallWithCodec(c.codec, "ObjectKind.UpsertObjectKind", req, &resp)
		return &resp, err
	}
	_, err := upsert(5)
	must.Eq(t, 409, errCode(t, err))
	resp, err := upsert(0)
	must.NoError(t, err)
	_, err = upsert(0)
	must.Eq(t, 409, errCode(t, err))
	_, err = upsert(resp.Index)
	must.NoError(t, err)
}

const testPGSchemaV2 = `{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "required": ["version", "storage"],
      "additionalProperties": false,
      "properties": {
        "version":  {"type": "string"},
        "replicas": {"type": "integer", "default": 1},
        "storage":  {"type": "object", "required": ["size"], "properties": {"size": {"type": "string"}, "class": {"type": "string", "default": "standard"}}},
        "backup":   {"type": "object"}
      }
    },
    "status": {
      "type": "object",
      "properties": {
        "observedGeneration": {"type": "integer"},
        "conditions": {"type": "array"}
      }
    }
  }
}`

func TestObjectKindMigration(t *testing.T) {
	ci.Parallel(t)

	s, cleanupS := TestServer(t, nil)
	defer cleanupS()
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	testutil.WaitForLeader(t, s.RPC)

	kind := testPGKind(true)
	kind.Rules = kind.Rules[:1]
	_, err := c.upsertKind(kind, false, "")
	must.NoError(t, err)
	for _, name := range []string{"a", "b", "c"} {
		_, err := c.apply(testPGObject("default", name, map[string]any{"version": "16", "storage": "100GiB"}), nil, false, "")
		must.NoError(t, err)
	}
	_, err = c.status("default", "a", map[string]any{"observedGeneration": 1, "readyReplicas": 1}, nil, "")
	must.NoError(t, err)

	// The new schema makes the stored objects invalid: rejected, the dry
	// run reports every object.
	v2 := testPGKind(true)
	v2.Rules = nil
	v2.Schema = testPGSchemaV2
	resp, err := c.upsertKind(v2, true, "")
	must.NoError(t, err)
	must.NotNil(t, resp.Migration)
	must.Eq(t, 3, resp.Migration.Objects)
	must.Eq(t, 3, resp.Migration.Failed)
	_, err = c.upsertKind(v2, false, "")
	must.Eq(t, 409, errCode(t, err))
	must.ErrorContains(t, err, "raise the version and add a migration")

	// A lower version is rejected.
	v0 := v2.Copy()
	v0.Schema = testPGSchema
	v0.Version = 1
	v2.Version = 2
	v2.Migration = &structs.ObjectKindMigration{
		Spec:   `self.spec.omit(["storage", "backup"]).merge({"storage": {"size": self.spec.storage}})`,
		Status: `self.status.omit(["readyReplicas"])`,
	}
	// The status expression fails on objects without a status: the report
	// names them.
	resp, err = c.upsertKind(v2, true, "")
	must.NoError(t, err)
	must.Eq(t, 2, resp.Migration.Failed)

	v2.Migration.Status = `has(self.status) ? self.status.omit(["readyReplicas"]) : {}`
	resp, err = c.upsertKind(v2, true, "")
	must.NoError(t, err)
	must.Eq(t, 0, resp.Migration.Failed)
	must.Eq(t, 3, resp.Migration.Pending)
	must.Eq(t, 3, resp.Migration.Changed)
	must.Len(t, structs.MaxObjectMigrationSamples, resp.Migration.Samples)
	sample := resp.Migration.Samples[0]
	must.Eq(t, "100GiB", sample.SpecBefore["storage"])
	must.Eq(t, map[string]any{"size": "100GiB", "class": "standard"}, sample.SpecAfter["storage"].(map[string]any))

	// Nothing was written by the dry runs.
	obj, err := c.get("default", "a", "")
	must.NoError(t, err)
	must.Eq(t, "100GiB", obj.Spec["storage"])

	resp, err = c.upsertKind(v2, false, "")
	must.NoError(t, err)
	_, err = c.upsertKind(v0, false, "")
	must.ErrorContains(t, err, "lower than the stored version")

	// A write of an object the background migration hasn't reached yet
	// migrates it first.
	out, err := c.status("default", "a", map[string]any{"observedGeneration": 1}, nil, "")
	must.NoError(t, err)
	must.Eq(t, uint64(2), out.Object.KindVersion)
	must.Eq(t, "100GiB", out.Object.Spec["storage"].(map[string]any)["size"])
	must.Eq(t, uint64(2), out.Object.Generation)

	// The background migration converts the rest.
	stopCh := make(chan struct{})
	defer close(stopCh)
	must.NoError(t, s.migrateObjectsOnce(stopCh, map[string]string{}))
	for _, name := range []string{"b", "c"} {
		obj, err := c.get("default", name, "")
		must.NoError(t, err)
		must.Eq(t, uint64(2), obj.KindVersion)
		must.Eq(t, "standard", obj.Spec["storage"].(map[string]any)["class"])
		must.Eq(t, uint64(2), obj.Generation)
	}

	// New objects are written at the current version, in the new format.
	out, err = c.apply(testPGObject("default", "d", map[string]any{"version": "17", "storage": map[string]any{"size": "1Gi"}}), nil, false, "")
	must.NoError(t, err)
	must.Eq(t, uint64(2), out.Object.KindVersion)
	_, err = c.apply(testPGObject("default", "e", map[string]any{"version": "17", "storage": "1Gi"}), nil, false, "")
	must.Eq(t, 400, errCode(t, err))
}

func TestObjectKindMigration_FailureRecorded(t *testing.T) {
	ci.Parallel(t)

	s, cleanupS := TestServer(t, nil)
	defer cleanupS()
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	testutil.WaitForLeader(t, s.RPC)

	kind := testPGKind(true)
	kind.Rules = nil
	_, err := c.upsertKind(kind, false, "")
	must.NoError(t, err)
	_, err = c.apply(testPGObject("default", "a", map[string]any{"version": "16"}), nil, false, "")
	must.NoError(t, err)

	// A migration that passes the check at write time but fails later (an
	// object written by an older server, simulated by a direct state
	// write) is reported in the object's event log and not retried.
	v2 := kind.Copy()
	v2.Version = 2
	v2.Migration = &structs.ObjectKindMigration{Spec: `self.spec.merge({"replicas": int(self.spec.version) / (self.spec.replicas - 1)})`}
	resp, err := c.upsertKind(v2, true, "")
	must.NoError(t, err)
	must.Eq(t, 1, resp.Migration.Failed)
	must.StrContains(t, resp.Migration.Failures[0].Error, "division by zero")

	// Force it in: write the kind directly.
	stored, _ := s.State().ObjectKindByName(nil, kind.Group, kind.Kind)
	forced := v2.Copy()
	forced.Canonicalize()
	forced.CreateIndex = stored.CreateIndex
	must.NoError(t, s.State().UpsertObjectKind(structs.MsgTypeTestSetup, stored.ModifyIndex+100, forced))

	failed := map[string]string{}
	stopCh := make(chan struct{})
	defer close(stopCh)
	must.NoError(t, s.migrateObjectsOnce(stopCh, failed))
	must.MapLen(t, 1, failed)
	logReq := &structs.CustomObjectSpecificRequest{Group: kind.Group, Kind: kind.Kind, Name: "a",
		QueryOptions: structs.QueryOptions{Region: "global", Namespace: "default"}}
	var logResp structs.CustomObjectEventsResponse
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "CustomObject.ListEvents", logReq, &logResp))
	must.Len(t, 1, logResp.Events)
	must.Eq(t, structs.ObjectMigrationFailedReason, logResp.Events[0].Reason)

	// The next round skips it. (The leader's own migration loop may have
	// recorded the failure once as well; repetitions are counted.)
	must.NoError(t, s.migrateObjectsOnce(stopCh, failed))
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "CustomObject.ListEvents", logReq, &logResp))
	must.Len(t, 1, logResp.Events)
	must.LessEq(t, 2, logResp.Events[0].Count)
}

func TestObjectKindLimits(t *testing.T) {
	ci.Parallel(t)

	s, root, cleanupS := TestACLServer(t, nil)
	defer cleanupS()
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	testutil.WaitForLeader(t, s.RPC)

	rules := `namespace "default" { object "db.example.com/*" { capabilities = ["read", "write", "write-status"] } }`
	ctrl := mock.CreatePolicyAndToken(t, s.State(), 1001, "controller", rules)
	user := mock.CreatePolicyAndToken(t, s.State(), 1003, "user", rules)

	kind := testPGKind(true)
	kind.Limits = &structs.ObjectKindLimits{IdentityWriteRate: 0.001, IdentityWriteBurst: 3, MaxObjectsPerNamespace: 4}
	_, err := c.upsertKind(kind, false, root.SecretID)
	must.NoError(t, err)

	// The controller's burst is used up; the user's writes still pass.
	for i := range 3 {
		_, err := c.apply(testPGObject("default", "c"+string(rune('a'+i)), map[string]any{"version": "16"}), nil, false, ctrl.SecretID)
		must.NoError(t, err)
	}
	_, err = c.status("default", "ca", map[string]any{"observedGeneration": 1}, nil, ctrl.SecretID)
	must.Eq(t, 429, errCode(t, err))
	must.ErrorContains(t, err, "object limit exceeded")
	_, err = c.apply(testPGObject("default", "ua", map[string]any{"version": "16"}), nil, false, user.SecretID)
	must.NoError(t, err)

	// The fifth object of the namespace is over the limit.
	_, err = c.apply(testPGObject("default", "ub", map[string]any{"version": "16"}), nil, false, user.SecretID)
	must.Eq(t, 429, errCode(t, err))
	must.ErrorContains(t, err, "4 objects")

	// Changing the limits resets the buckets.
	kind.Limits = &structs.ObjectKindLimits{WriteRate: 0.001, WriteBurst: 1}
	_, err = c.upsertKind(kind, false, root.SecretID)
	must.NoError(t, err)
	_, err = c.status("default", "ca", map[string]any{"observedGeneration": 1}, nil, ctrl.SecretID)
	must.NoError(t, err)
	err = c.delete("default", "cb", nil, user.SecretID)
	must.Eq(t, 429, errCode(t, err))
}

func TestObjectKindLimits_Watches(t *testing.T) {
	ci.Parallel(t)

	s, cleanupS := TestServer(t, nil)
	defer cleanupS()
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	testutil.WaitForLeader(t, s.RPC)

	kind := testPGKind(true)
	kind.Limits = &structs.ObjectKindLimits{MaxWatchesPerIdentity: 1}
	_, err := c.upsertKind(kind, false, "")
	must.NoError(t, err)
	out, err := c.apply(testPGObject("default", "a", map[string]any{"version": "16"}), nil, false, "")
	must.NoError(t, err)

	watch := func(wait time.Duration) error {
		req := &structs.CustomObjectListRequest{Group: kind.Group, Kind: kind.Kind,
			QueryOptions: structs.QueryOptions{Region: "global", Namespace: "default", MinQueryIndex: out.Index + 1000, MaxQueryTime: wait}}
		var resp structs.CustomObjectListResponse
		return msgpackrpc.CallWithCodec(rpcClient(t, s), "CustomObject.List", req, &resp)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- watch(3 * time.Second) }()

	// Wait until the first watch holds its slot.
	open := func() int {
		s.objects.eco.lock.Lock()
		defer s.objects.eco.lock.Unlock()
		return len(s.objects.eco.watches)
	}
	for deadline := time.Now().Add(2 * time.Second); open() == 0 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	must.Eq(t, 1, open())

	// A second concurrent watch of the same identity is rejected.
	err = watch(50 * time.Millisecond)
	must.Eq(t, 429, errCode(t, err))
	must.ErrorContains(t, err, "1 watches")
	must.NoError(t, <-errCh)
	must.Eq(t, 0, open())

	// Afterwards it is allowed again; queries that don't block don't count.
	must.NoError(t, watch(50*time.Millisecond))
	_, err = c.list("default", nil, "", "")
	must.NoError(t, err)
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/stream"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/secretstore"
	"github.com/bacanoo-io/norca/norca/testutil"
)

// engineTest is a server with an ACL root token.
type engineTest struct {
	t    *testing.T
	s    *Server
	root *structs.ACLToken
}

func newEngineTest(t *testing.T, cb func(*Config)) *engineTest {
	t.Helper()
	s, root, cleanup := secretTestServer(t, true, func(c *Config) {
		c.secretLeaseWatcherDisabled = true
		if cb != nil {
			cb(c)
		}
	})
	t.Cleanup(cleanup)
	return &engineTest{t: t, s: s, root: root}
}

func (et *engineTest) engine() *structs.SecretEngine { return operatorEngine() }

func (et *engineTest) upsert(token string, engine *structs.SecretEngine) (*structs.SecretEngineWriteResponse, error) {
	var resp structs.SecretEngineWriteResponse
	err := et.s.RPC(structs.SecretEngineUpsertRPCMethod, &structs.SecretEngineUpsertRequest{
		Engine:       engine,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: token},
	}, &resp)
	return &resp, err
}

// alloc registers a running allocation whose task "web" asks for the
// worker role of the nats engine and, if grant is set, has the
// secret_access binding to the credentials path. It returns the
// allocation and the task's workload identity.
func (et *engineTest) alloc(grant bool) (*structs.Allocation, string) {
	t := et.t
	alloc := mock.Alloc()
	alloc.ClientStatus = structs.AllocClientStatusRunning
	task := alloc.Job.TaskGroups[0].Tasks[0]
	task.Secrets = []*structs.TaskSecret{{Name: "pg", Engine: "pg", Role: "worker", Env: map[string]string{"PW": "password"}}}
	if grant {
		task.SecretAccess = []*structs.SecretAccess{{Path: "engines/pg/creds/*"}}
	}
	store := et.s.fsm.State()
	index, _ := store.LatestIndex()
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, index+1, []*structs.Allocation{alloc}))
	token, _, err := et.s.encrypter.SignClaims(alloc.ToTaskIdentityClaims(alloc.Job, task.Name))
	must.NoError(t, err)
	return alloc, token
}

func (et *engineTest) create(token string) (*structs.SecretLeaseCreateResponse, error) {
	var resp structs.SecretLeaseCreateResponse
	err := et.s.RPC(structs.SecretLeaseCreateRPCMethod, &structs.SecretLeaseCreateRequest{
		Engine: "pg", Role: "worker", SecretName: "pg",
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: token},
	}, &resp)
	return &resp, err
}

func (et *engineTest) lease(id string) *structs.SecretLease {
	l, err := et.s.fsm.State().SecretLeaseByID(nil, id)
	must.NoError(et.t, err)
	return l
}

// testLeaseWatcher is a lease watcher with a settable clock.
func testLeaseWatcher(s *Server, now func() time.Time) *secretLeaseWatcher {
	w := newSecretLeaseWatcher(s)
	w.now = now
	return w
}

func TestSecretEngineEndpoint_CRUDAndACL(t *testing.T) {
	ci.Parallel(t)
	et := newEngineTest(t, nil)
	store := et.s.fsm.State()
	reader := mock.CreatePolicyAndToken(t, store, 1001, "engine-reader",
		`namespace "default" { capabilities = ["read-secret-engine"] }`)
	writer := mock.CreatePolicyAndToken(t, store, 1002, "engine-writer",
		`namespace "default" { capabilities = ["write-secret-engine", "read-secret-engine"] }`)
	op := startMockOperator(t, et, "")

	_, err := et.upsert(reader.SecretID, et.engine())
	must.EqError(t, err, structs.ErrPermissionDenied.Error())

	// Invalid engines.
	bad := et.engine()
	bad.Roles[0].MaxTTL = time.Second
	_, err = et.upsert(writer.SecretID, bad)
	must.ErrorContains(t, err, "max_ttl must not be shorter than ttl")

	resp, err := et.upsert(writer.SecretID, et.engine())
	must.NoError(t, err)
	must.Eq(t, "pg", resp.Engine.Name)
	must.Eq(t, structs.DefaultNamespace, resp.Engine.Namespace)

	upd := et.engine()
	upd.Description = "orders"
	resp, err = et.upsert(writer.SecretID, upd)
	must.NoError(t, err)
	must.Eq(t, "orders", resp.Engine.Description)
	typ := et.engine()
	typ.Type = "postgresql"
	_, err = et.upsert(writer.SecretID, typ)
	must.Error(t, err)

	var list structs.SecretEngineListResponse
	must.NoError(t, et.s.RPC(structs.SecretEngineListRPCMethod,
		&structs.SecretEngineListRequest{QueryOptions: secretQuery(reader.SecretID)}, &list))
	must.Len(t, 1, list.Engines)
	must.Eq(t, []string{"worker"}, list.Engines[0].Roles)
	var read structs.SecretEngineResponse
	must.NoError(t, et.s.RPC(structs.SecretEngineReadRPCMethod,
		&structs.SecretEngineReadRequest{Name: "pg", QueryOptions: secretQuery(reader.SecretID)}, &read))
	must.Eq(t, "orders", read.Engine.Description)
	none := mock.CreatePolicyAndToken(t, store, 1004, "none", `namespace "default" { capabilities = ["list-secrets"] }`)
	err = et.s.RPC(structs.SecretEngineReadRPCMethod,
		&structs.SecretEngineReadRequest{Name: "pg", QueryOptions: secretQuery(none.SecretID)}, &read)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())

	// No delete while a lease is live.
	_, wi := et.alloc(true)
	created, err := et.create(wi)
	must.NoError(t, err)
	del := &structs.SecretEngineDeleteRequest{Name: "pg",
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: writer.SecretID}}
	var delResp structs.SecretEngineWriteResponse
	must.ErrorContains(t, et.s.RPC(structs.SecretEngineDeleteRPCMethod, del, &delResp), "live leases")

	var rev structs.SecretLeaseRevokeResponse
	err = et.s.RPC(structs.SecretLeaseRevokeRPCMethod, &structs.SecretLeaseRevokeRequest{Engine: "pg",
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: reader.SecretID}}, &rev)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())
	must.NoError(t, et.s.RPC(structs.SecretLeaseRevokeRPCMethod, &structs.SecretLeaseRevokeRequest{Engine: "pg",
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: writer.SecretID}}, &rev))
	must.Eq(t, []string{created.Lease.ID}, rev.Revoked)
	must.Eq(t, structs.SecretLeaseRevokeOperator, et.lease(created.Lease.ID).RevokeReason)

	// The delete removes the revoked leases, so it first sends their
	// revocations to the backend (the lease watcher, which does it
	// otherwise, is off here), and fails while that fails (review finding
	// SRV-17).
	ref := et.lease(created.Lease.ID).BackendRef
	must.NotEq(t, "", ref)
	must.False(t, op.isRevoked(ref))
	op.status.Store(http.StatusInternalServerError)
	must.ErrorContains(t, et.s.RPC(structs.SecretEngineDeleteRPCMethod, del, &delResp), "sending the revocations of its leases")
	e, err := store.SecretEngineByName(nil, structs.DefaultNamespace, "pg")
	must.NoError(t, err)
	must.NotNil(t, e)
	must.NotNil(t, et.lease(created.Lease.ID))
	op.status.Store(http.StatusOK)

	must.NoError(t, et.s.RPC(structs.SecretEngineDeleteRPCMethod, del, &delResp))
	must.True(t, op.isRevoked(ref), must.Sprint("the revocation reached the operator before the lease was removed"))
	e, err = store.SecretEngineByName(nil, structs.DefaultNamespace, "pg")
	must.NoError(t, err)
	must.Nil(t, e)
	must.Nil(t, et.lease(created.Lease.ID))
}

// TestSecretLease_Lifecycle creates a credential for a task, renews it up to
// its max_ttl, and checks the revocations of the lease watcher: an expired
// lease, a stopped allocation and max_ttl; the operator gets the revoked
// credentials.
func TestSecretLease_Lifecycle(t *testing.T) {
	ci.Parallel(t)
	et := newEngineTest(t, nil)
	op := startMockOperator(t, et, "")
	_, err := et.upsert(et.root.SecretID, et.engine())
	must.NoError(t, err)

	// Without a secret_access binding (or a policy bound to the job), the
	// task gets nothing; nor does an ACL token.
	_, noGrant := et.alloc(false)
	_, err = et.create(noGrant)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())
	_, err = et.create(et.root.SecretID)
	must.ErrorContains(t, err, "require a workload identity")

	alloc, wi := et.alloc(true)
	other := &structs.SecretLeaseCreateRequest{Engine: "pg", Role: "admin", SecretName: "pg",
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: wi}}
	var otherResp structs.SecretLeaseCreateResponse
	must.ErrorContains(t, et.s.RPC(structs.SecretLeaseCreateRPCMethod, other, &otherResp), "has no secret block")

	created, err := et.create(wi)
	must.NoError(t, err)
	lease := created.Lease
	must.Eq(t, alloc.ID, lease.AllocID)
	must.Eq(t, "web", lease.Task)
	must.Eq(t, "pg", lease.SecretName)
	must.Eq(t, "ref-"+lease.ID, lease.BackendRef)
	must.False(t, lease.Revoked)

	// Renewal extends the lease by its TTL, never beyond max_ttl.
	renew := func(id string) (*structs.SecretLease, error) {
		var r structs.SecretLeaseWriteResponse
		err := et.s.RPC(structs.SecretLeaseRenewRPCMethod, &structs.SecretLeaseRenewRequest{LeaseID: id,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: wi}}, &r)
		return r.Lease, err
	}
	time.Sleep(50 * time.Millisecond)
	renewed, err := renew(lease.ID)
	must.NoError(t, err)
	must.Greater(t, lease.ExpireTime, renewed.ExpireTime)
	must.Eq(t, 1, renewed.Renewals)
	_, err = renew("unknown")
	must.ErrorContains(t, err, "not found")
	// Another task's identity can't renew it.
	_, wi2 := et.alloc(true)
	var r2 structs.SecretLeaseWriteResponse
	err = et.s.RPC(structs.SecretLeaseRenewRPCMethod, &structs.SecretLeaseRenewRequest{LeaseID: lease.ID,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: wi2}}, &r2)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())

	// The watcher revokes the lease that expired without renewal and
	// sends the revocation to the operator.
	w := testLeaseWatcher(et.s, func() time.Time { return time.Unix(0, renewed.ExpireTime).Add(time.Millisecond) })
	w.evaluate(context.Background(), memdb.NewWatchSet())
	got := et.lease(lease.ID)
	must.True(t, got.Revoked)
	must.Eq(t, structs.SecretLeaseRevokeExpired, got.RevokeReason)
	must.True(t, op.isRevoked(lease.BackendRef))
	_, err = renew(lease.ID)
	must.ErrorContains(t, err, "revoked")

	// A stopped allocation: its lease is revoked at once.
	second, err := et.create(wi)
	must.NoError(t, err)
	stopped := alloc.Copy()
	stopped.ClientStatus = structs.AllocClientStatusComplete
	index, _ := et.s.fsm.State().LatestIndex()
	must.NoError(t, et.s.fsm.State().UpdateAllocsFromClient(structs.MsgTypeTestSetup, index+1, []*structs.Allocation{stopped}))
	w.now = time.Now
	w.evaluate(context.Background(), memdb.NewWatchSet())
	must.Eq(t, structs.SecretLeaseRevokeStopped, et.lease(second.Lease.ID).RevokeReason)
	must.True(t, op.isRevoked(second.Lease.BackendRef))
	_, err = et.create(wi)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())

	// max_ttl: a lease renewed up to its end is revoked for max_ttl, and
	// revoked leases are removed once their credential expired.
	alloc3, wi3 := et.alloc(true)
	third, err := et.create(wi3)
	must.NoError(t, err)
	l3 := third.Lease.Copy()
	l3.ExpireTime = l3.MaxExpireTime
	_, err = et.s.secretLeaseApply([]*structs.SecretLease{l3})
	must.NoError(t, err)
	w.now = func() time.Time { return time.Unix(0, l3.MaxExpireTime).Add(time.Millisecond) }
	w.evaluate(context.Background(), memdb.NewWatchSet())
	must.Eq(t, structs.SecretLeaseRevokeMaxTTL, et.lease(third.Lease.ID).RevokeReason)
	must.Eq(t, alloc3.ID, et.lease(third.Lease.ID).AllocID)
	w.now = func() time.Time { return time.Unix(0, l3.MaxExpireTime).Add(10 * time.Second) }
	w.evaluate(context.Background(), memdb.NewWatchSet())
	must.Nil(t, et.lease(third.Lease.ID))
	must.Nil(t, et.lease(lease.ID))
}

// TestSecretLease_LeaderChange checks that the leases are revoked after a
// leader change: the leases are in Raft, and the watcher of the new leader
// revokes the lease that is not renewed and sends the revocation to the
// operator.
func TestSecretLease_LeaderChange(t *testing.T) {
	ci.Parallel(t)
	var servers []*Server
	for i := 0; i < 3; i++ {
		s, cleanup := TestServer(t, func(c *Config) { c.BootstrapExpect = 3 })
		defer cleanup()
		servers = append(servers, s)
	}
	TestJoin(t, servers...)
	for _, s := range servers {
		testutil.WaitForLeader(t, s.RPC)
	}
	leaderOf := func(candidates []*Server) *Server {
		var leader *Server
		waitForSecretStore(t, "leader", func() bool {
			for _, s := range candidates {
				if s.IsLeader() {
					leader = s
				}
			}
			return leader != nil
		})
		waitForSecretCore(t, leader)
		return leader
	}
	leader := leaderOf(servers)

	et := &engineTest{t: t, s: leader}
	op := startMockOperator(t, et, "")
	// The next leader signs its client certificate with the CA of the region,
	// whose key is replicated in the background.
	root := waitForMTLSRoot(t, leader)
	waitForSecretStore(t, "keyring replicated", func() bool {
		for _, s := range servers {
			if _, err := s.encrypter.GetKey(root.SigningKeyID); err != nil {
				return false
			}
		}
		return true
	})
	for _, s := range servers {
		if s != leader {
			op.register(t, s)
		}
	}
	engine := et.engine()
	engine.Roles[0].TTL, engine.Roles[0].MaxTTL = 3*time.Second, time.Hour
	_, err := et.upsert("", engine)
	must.NoError(t, err)

	alloc := mock.Alloc()
	alloc.ClientStatus = structs.AllocClientStatusRunning
	alloc.Job.TaskGroups[0].Tasks[0].Secrets = []*structs.TaskSecret{{Name: "pg", Engine: "pg", Role: "worker", Env: map[string]string{"PW": "password"}}}
	for _, s := range servers {
		// The allocation is put into every server's state directly.
		index, _ := s.fsm.State().LatestIndex()
		must.NoError(t, s.fsm.State().UpsertAllocs(structs.MsgTypeTestSetup, index+1, []*structs.Allocation{alloc.Copy()}))
	}
	wi, _, err := leader.encrypter.SignClaims(alloc.ToTaskIdentityClaims(alloc.Job, "web"))
	must.NoError(t, err)
	created, err := et.create(wi)
	must.NoError(t, err)

	// The leader goes away before the lease expires.
	var rest []*Server
	for _, s := range servers {
		if s != leader {
			rest = append(rest, s)
		}
	}
	must.NoError(t, leader.Shutdown())
	next := leaderOf(rest)
	waitForSecretStore(t, "lease revoked by the new leader", func() bool {
		l, err := next.State().SecretLeaseByID(nil, created.Lease.ID)
		return err == nil && l != nil && l.Revoked
	})
	l, _ := next.State().SecretLeaseByID(nil, created.Lease.ID)
	must.Eq(t, structs.SecretLeaseRevokeExpired, l.RevokeReason)
	waitForSecretStore(t, "revocation sent", func() bool { return op.isRevoked(l.BackendRef) })
}

// TestSecretLease_AuditAndRedaction checks the audit records and events of
// the lease operations and that neither a credential nor the admin
// credential appears in records, events, listings, the audit file or the
// server log.
func TestSecretLease_AuditAndRedaction(t *testing.T) {
	ci.Parallel(t)
	auditPath := filepath.Join(t.TempDir(), "secrets-audit.log")
	logs := &syncBuffer{}
	et := newEngineTest(t, func(c *Config) {
		c.SecretsAudit = &secretstore.AuditFileConfig{Path: auditPath}
		c.Logger = hclog.NewInterceptLogger(&hclog.LoggerOptions{Name: "server", Level: hclog.Trace, Output: logs})
	})
	broker, err := et.s.fsm.State().EventBroker()
	must.NoError(t, err)
	sub, err := broker.Subscribe(&stream.SubscribeRequest{
		Topics:    map[structs.Topic][]string{structs.TopicSecret: {"*"}, structs.TopicSecretAccess: {"*"}},
		Namespace: "*",
	})
	must.NoError(t, err)
	defer sub.Unsubscribe()

	startMockOperator(t, et, "")
	_, err = et.upsert(et.root.SecretID, et.engine())
	must.NoError(t, err)
	_, wi := et.alloc(true)
	created, err := et.create(wi)
	must.NoError(t, err)
	var rr structs.SecretLeaseWriteResponse
	must.NoError(t, et.s.RPC(structs.SecretLeaseRenewRPCMethod, &structs.SecretLeaseRenewRequest{LeaseID: created.Lease.ID,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: wi}}, &rr))
	var rev structs.SecretLeaseRevokeResponse
	must.NoError(t, et.s.RPC(structs.SecretLeaseRevokeRPCMethod, &structs.SecretLeaseRevokeRequest{LeaseID: created.Lease.ID,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: wi}}, &rev))

	var list structs.SecretLeaseListResponse
	must.NoError(t, et.s.RPC(structs.SecretLeaseListRPCMethod,
		&structs.SecretLeaseListRequest{Engine: "pg", QueryOptions: secretQuery(et.root.SecretID)}, &list))
	must.Len(t, 1, list.Leases)

	var access []*structs.SecretAccessEvent
	var types []string
	var outputs []any
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	want := map[string]bool{structs.TypeSecretLeaseCreated: false, structs.TypeSecretLeaseRenewed: false,
		structs.TypeSecretLeaseRevoked: false, structs.TypeSecretEngineUpserted: false}
	done := func() bool {
		for _, v := range want {
			if !v {
				return false
			}
		}
		ops := map[string]bool{}
		for _, a := range access {
			ops[a.Operation] = true
		}
		return ops[structs.SecretOpLeaseRevoke] && ops[structs.SecretOpLeaseRenew] && ops[structs.SecretOpLeaseCreate]
	}
	for !done() {
		events, err := sub.Next(ctx)
		must.NoError(t, err)
		for _, e := range events.Events {
			outputs = append(outputs, e)
			types = append(types, e.Type)
			if _, ok := want[e.Type]; ok {
				want[e.Type] = true
			}
			if rec, ok := e.Payload.(*structs.SecretAccessEvent); ok && rec.Result == structs.SecretAccessResultOK {
				access = append(access, rec)
			}
		}
	}
	var create *structs.SecretAccessEvent
	for _, a := range access {
		if a.Operation == structs.SecretOpLeaseCreate {
			create = a
		}
	}
	must.NotNil(t, create)
	must.Eq(t, "engines/pg/creds/worker", create.Path)
	must.Eq(t, created.Lease.ID, create.LeaseID)
	must.Eq(t, "pg", create.Engine)
	must.Eq(t, "worker", create.Role)
	must.Eq(t, "web", create.Accessor.Task)

	outputs = append(outputs, list, rr, rev, created.Lease)
	canaries := []string{created.Items["password"]}
	for i, out := range outputs {
		data, err := json.Marshal(out)
		must.NoError(t, err)
		for _, c := range canaries {
			must.StrNotContains(t, string(data), c, must.Sprintf("output %d", i))
		}
	}
	audit, err := os.ReadFile(auditPath)
	must.NoError(t, err)
	must.StrContains(t, string(audit), structs.SecretOpLeaseCreate)
	for _, c := range canaries {
		must.StrNotContains(t, string(audit), c)
		must.StrNotContains(t, logs.String(), c)
	}
	must.True(t, strings.Contains(strings.Join(types, ","), structs.TypeSecretLeaseCreated), must.Sprint(types))
	_ = fmt.Sprint
}

// A management token can delete an engine with force although the
// revocations of its leases can't be sent to the backend (the operator is
// gone for good); nobody else can, and without force the delete still
// fails.
func TestSecretEngineEndpoint_ForceDelete(t *testing.T) {
	ci.Parallel(t)
	et := newEngineTest(t, nil)
	store := et.s.fsm.State()
	writer := mock.CreatePolicyAndToken(t, store, 1002, "engine-writer",
		`namespace "default" { capabilities = ["write-secret-engine", "read-secret-engine"] }`)
	op := startMockOperator(t, et, "")
	_, err := et.upsert(et.root.SecretID, et.engine())
	must.NoError(t, err)

	del := func(token string, force bool) error {
		var resp structs.SecretEngineWriteResponse
		return et.s.RPC(structs.SecretEngineDeleteRPCMethod, &structs.SecretEngineDeleteRequest{Name: "pg", Force: force,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: token}}, &resp)
	}
	engine := func() *structs.SecretEngine {
		e, err := store.SecretEngineByName(nil, structs.DefaultNamespace, "pg")
		must.NoError(t, err)
		return e
	}

	_, wi := et.alloc(true)
	created, err := et.create(wi)
	must.NoError(t, err)

	// Force does not replace revoking the live leases.
	must.ErrorContains(t, del(et.root.SecretID, true), "live leases")

	var rev structs.SecretLeaseRevokeResponse
	must.NoError(t, et.s.RPC(structs.SecretLeaseRevokeRPCMethod, &structs.SecretLeaseRevokeRequest{Engine: "pg",
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: writer.SecretID}}, &rev))
	ref := et.lease(created.Lease.ID).BackendRef

	// The operator is gone: the revocation can't be delivered.
	op.status.Store(http.StatusInternalServerError)
	must.ErrorContains(t, del(writer.SecretID, false), "sending the revocations of its leases")
	must.ErrorContains(t, del(et.root.SecretID, false), "sending the revocations of its leases")
	must.EqError(t, del(writer.SecretID, true), structs.ErrPermissionDenied.Error())
	must.NotNil(t, engine())
	must.NotNil(t, et.lease(created.Lease.ID))

	must.NoError(t, del(et.root.SecretID, true))
	must.Nil(t, engine())
	must.Nil(t, et.lease(created.Lease.ID))
	must.False(t, op.isRevoked(ref))
}

// A forced delete always leaves a trace: a warning in the server log with
// the caller's accessor and the mark in the audit record, also when nothing
// was left undelivered. Leases that expired without being revoked block the
// delete without force, and are named in the log with it.
func TestSecretEngineEndpoint_ForceDeleteTrace(t *testing.T) {
	ci.Parallel(t)
	logs := &syncBuffer{}
	et := newEngineTest(t, func(c *Config) {
		c.Logger = hclog.NewInterceptLogger(&hclog.LoggerOptions{Name: "server", Level: hclog.Trace, Output: logs})
	})
	store := et.s.fsm.State()
	broker, err := store.EventBroker()
	must.NoError(t, err)
	sub, err := broker.Subscribe(&stream.SubscribeRequest{
		Topics:    map[structs.Topic][]string{structs.TopicSecretAccess: {"*"}},
		Namespace: "*",
	})
	must.NoError(t, err)
	defer sub.Unsubscribe()
	startMockOperator(t, et, "")

	del := func(force bool) error {
		var resp structs.SecretEngineWriteResponse
		return et.s.RPC(structs.SecretEngineDeleteRPCMethod, &structs.SecretEngineDeleteRequest{Name: "pg", Force: force,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: et.root.SecretID}}, &resp)
	}
	// deletes returns the audit records of the engine deletes so far.
	var deletes []*structs.SecretAccessEvent
	audited := func(n int) []*structs.SecretAccessEvent {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for len(deletes) < n {
			events, err := sub.Next(ctx)
			must.NoError(t, err)
			for _, e := range events.Events {
				if rec, ok := e.Payload.(*structs.SecretAccessEvent); ok && rec.Operation == structs.SecretOpEngineDelete &&
					rec.Result == structs.SecretAccessResultOK {
					deletes = append(deletes, rec)
				}
			}
		}
		return deletes
	}

	// Nothing to deliver: the forced delete is logged and audited all the
	// same, a normal one is not marked.
	_, err = et.upsert(et.root.SecretID, et.engine())
	must.NoError(t, err)
	must.NoError(t, del(false))
	must.False(t, audited(1)[0].Force)
	must.StrNotContains(t, logs.String(), "deleted with force")

	_, err = et.upsert(et.root.SecretID, et.engine())
	must.NoError(t, err)
	must.NoError(t, del(true))
	must.True(t, audited(2)[1].Force)
	must.Eq(t, et.root.AccessorID, audited(2)[1].Accessor.AccessorID)
	must.StrContains(t, logs.String(), "secret engine deleted with force")
	must.StrContains(t, logs.String(), "accessor_id="+et.root.AccessorID)

	// A lease that expired without being revoked (the lease watcher is off
	// here): its credential may still be valid.
	_, err = et.upsert(et.root.SecretID, et.engine())
	must.NoError(t, err)
	_, wi := et.alloc(true)
	created, err := et.create(wi)
	must.NoError(t, err)
	expired := et.lease(created.Lease.ID).Copy()
	expired.ExpireTime = time.Now().Add(-time.Minute).UnixNano()
	must.Greater(t, time.Now().UnixNano(), expired.MaxExpireTime)
	_, err = et.s.secretLeaseApply([]*structs.SecretLease{expired})
	must.NoError(t, err)
	must.False(t, et.lease(created.Lease.ID).Revoked)

	must.ErrorContains(t, del(false), "expired leases that are not revoked yet")
	must.NotNil(t, et.lease(created.Lease.ID))
	must.NoError(t, del(true))
	e, err := store.SecretEngineByName(nil, structs.DefaultNamespace, "pg")
	must.NoError(t, err)
	must.Nil(t, e)
	must.Nil(t, et.lease(created.Lease.ID))
	must.True(t, audited(3)[2].Force)
	must.StrContains(t, logs.String(), "leases that expired were never revoked")
	must.StrContains(t, logs.String(), created.Lease.ID)
}

// A lease created while the delete of its engine sends the pending
// revocations to the backend is not purged with the engine: the delete
// fails, and the lease and its credential stay accounted for.
func TestSecretEngineEndpoint_DeleteRacesLeaseCreate(t *testing.T) {
	ci.Parallel(t)
	et := newEngineTest(t, nil)
	store := et.s.fsm.State()
	op := startMockOperator(t, et, "")
	_, err := et.upsert(et.root.SecretID, et.engine())
	must.NoError(t, err)

	// A revoked lease: the delete has a revocation to send.
	_, wi := et.alloc(true)
	first, err := et.create(wi)
	must.NoError(t, err)
	var rev structs.SecretLeaseRevokeResponse
	must.NoError(t, et.s.RPC(structs.SecretLeaseRevokeRPCMethod, &structs.SecretLeaseRevokeRequest{Engine: "pg",
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: et.root.SecretID}}, &rev))
	must.Eq(t, []string{first.Lease.ID}, rev.Revoked)

	// The operator answers slowly: the delete stays in the call that sends
	// the revocation while another task asks for a credential.
	op.delay = time.Second
	delErr := make(chan error, 1)
	go func() {
		var resp structs.SecretEngineWriteResponse
		delErr <- et.s.RPC(structs.SecretEngineDeleteRPCMethod, &structs.SecretEngineDeleteRequest{Name: "pg",
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: et.root.SecretID}}, &resp)
	}()
	time.Sleep(300 * time.Millisecond)
	_, wi2 := et.alloc(true)
	second, createErr := et.create(wi2)
	err = <-delErr

	must.ErrorContains(t, err, "live leases", must.Sprint("the delete of an engine that got a lease meanwhile"))
	must.NoError(t, createErr)
	engine, err := store.SecretEngineByName(nil, structs.DefaultNamespace, "pg")
	must.NoError(t, err)
	must.NotNil(t, engine)
	lease := et.lease(second.Lease.ID)
	must.NotNil(t, lease)
	must.True(t, lease.Live(time.Now()))
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/nomad/ci"
	"github.com/bacanoo-io/norca/nomad/nomad/mock"
	"github.com/bacanoo-io/norca/nomad/nomad/stream"
	"github.com/bacanoo-io/norca/nomad/nomad/structs"
	"github.com/bacanoo-io/norca/nomad/secretstore"
	"github.com/bacanoo-io/norca/nomad/testutil"
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
	startMockOperator(t, et, "")

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

	must.NoError(t, et.s.RPC(structs.SecretEngineDeleteRPCMethod, del, &delResp))
	e, err := store.SecretEngineByName(nil, structs.DefaultNamespace, "pg")
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

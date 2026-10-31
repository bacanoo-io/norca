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
	"github.com/nats-io/nats.go"
	"github.com/shoenig/test/must"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/secretengine"
	"github.com/hashicorp/nomad/nomad/secretengine/natstest"
	"github.com/hashicorp/nomad/nomad/stream"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/secretstore"
	"github.com/hashicorp/nomad/testutil"
)

// engineTest is a server with an ACL root token, an embedded NATS server in
// operator mode and the admin credential of a nats engine stored at
// "nats/admin".
type engineTest struct {
	t    *testing.T
	s    *Server
	root *structs.ACLToken
	nats *natstest.Server
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
	et := &engineTest{t: t, s: s, root: root, nats: natstest.Start(t)}
	var w structs.SecretWriteResponse
	must.NoError(t, s.RPC(structs.SecretUpsertRPCMethod,
		secretWriteReq("nats/admin", root.SecretID, et.nats.Credential()), &w))
	return et
}

func (et *engineTest) engine() *structs.SecretEngine {
	return &structs.SecretEngine{
		Name:           "nats",
		Type:           structs.SecretEngineTypeNATS,
		CredentialPath: "nats/admin",
		NATS:           &structs.SecretEngineNATS{Servers: []string{et.nats.URL}, AccountJWT: et.nats.AccountJWT},
		Roles: []*structs.SecretEngineRole{{
			Name: "worker", TTL: 2 * time.Second, MaxTTL: 5 * time.Second,
			NATS: &structs.SecretRoleNATS{Publish: []string{"orders.>"}, Subscribe: []string{"orders.>"}},
		}},
	}
}

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
	task.Secrets = []*structs.TaskSecret{{Name: "nats", Engine: "nats", Role: "worker"}}
	if grant {
		task.SecretAccess = []*structs.SecretAccess{{Path: "engines/nats/creds/*"}}
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
		Engine: "nats", Role: "worker", SecretName: "nats",
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: token},
	}, &resp)
	return &resp, err
}

func (et *engineTest) lease(id string) *structs.SecretLease {
	l, err := et.s.fsm.State().SecretLeaseByID(nil, id)
	must.NoError(et.t, err)
	return l
}

// connect connects to the NATS server with a credential's items.
func (et *engineTest) connect(items structs.SecretItems) error {
	nc, err := nats.Connect(et.nats.URL, nats.NoReconnect(), nats.Timeout(5*time.Second),
		nats.UserJWTAndSeed(items[secretengine.NATSItemJWT], items[secretengine.NATSItemSeed]))
	if err == nil {
		nc.Close()
	}
	return err
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
	owner := mock.CreatePolicyAndToken(t, store, 1003, "engine-owner",
		`namespace "default" {
		   capabilities = ["write-secret-engine"]
		   secret "nats/*" { capabilities = ["read-secret"] }
		 }`)

	// Naming the admin credential needs read-secret on it.
	_, err := et.upsert(reader.SecretID, et.engine())
	must.EqError(t, err, structs.ErrPermissionDenied.Error())
	_, err = et.upsert(writer.SecretID, et.engine())
	must.EqError(t, err, structs.ErrPermissionDenied.Error())

	// Invalid engines.
	bad := et.engine()
	bad.Roles[0].MaxTTL = time.Second
	_, err = et.upsert(owner.SecretID, bad)
	must.ErrorContains(t, err, "max_ttl must not be shorter than ttl")
	bad = et.engine()
	bad.CredentialPath = "nats/missing"
	_, err = et.upsert(owner.SecretID, bad)
	must.ErrorContains(t, err, "is not readable")
	bad = et.engine()
	bad.NATS.Servers = []string{"nats://127.0.0.1:1"}
	_, err = et.upsert(owner.SecretID, bad)
	must.ErrorContains(t, err, "configuring the backend failed")

	resp, err := et.upsert(owner.SecretID, et.engine())
	must.NoError(t, err)
	must.Eq(t, "nats", resp.Engine.Name)
	must.Eq(t, structs.DefaultNamespace, resp.Engine.Namespace)

	// An update keeps the credential path: write-secret-engine suffices.
	upd := et.engine()
	upd.Description = "orders"
	resp, err = et.upsert(writer.SecretID, upd)
	must.NoError(t, err)
	must.Eq(t, "orders", resp.Engine.Description)
	typ := et.engine()
	typ.Type = "postgresql"
	_, err = et.upsert(owner.SecretID, typ)
	must.Error(t, err)

	var list structs.SecretEngineListResponse
	must.NoError(t, et.s.RPC(structs.SecretEngineListRPCMethod,
		&structs.SecretEngineListRequest{QueryOptions: secretQuery(reader.SecretID)}, &list))
	must.Len(t, 1, list.Engines)
	must.Eq(t, []string{"worker"}, list.Engines[0].Roles)
	var read structs.SecretEngineResponse
	must.NoError(t, et.s.RPC(structs.SecretEngineReadRPCMethod,
		&structs.SecretEngineReadRequest{Name: "nats", QueryOptions: secretQuery(reader.SecretID)}, &read))
	must.Eq(t, "orders", read.Engine.Description)
	none := mock.CreatePolicyAndToken(t, store, 1004, "none", `namespace "default" { capabilities = ["list-secrets"] }`)
	err = et.s.RPC(structs.SecretEngineReadRPCMethod,
		&structs.SecretEngineReadRequest{Name: "nats", QueryOptions: secretQuery(none.SecretID)}, &read)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())

	// No delete while a lease is live.
	_, wi := et.alloc(true)
	created, err := et.create(wi)
	must.NoError(t, err)
	del := &structs.SecretEngineDeleteRequest{Name: "nats",
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: writer.SecretID}}
	var delResp structs.SecretEngineWriteResponse
	must.ErrorContains(t, et.s.RPC(structs.SecretEngineDeleteRPCMethod, del, &delResp), "live leases")

	var rev structs.SecretLeaseRevokeResponse
	err = et.s.RPC(structs.SecretLeaseRevokeRPCMethod, &structs.SecretLeaseRevokeRequest{Engine: "nats",
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: reader.SecretID}}, &rev)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())
	must.NoError(t, et.s.RPC(structs.SecretLeaseRevokeRPCMethod, &structs.SecretLeaseRevokeRequest{Engine: "nats",
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: writer.SecretID}}, &rev))
	must.Eq(t, []string{created.Lease.ID}, rev.Revoked)
	must.Eq(t, structs.SecretLeaseRevokeOperator, et.lease(created.Lease.ID).RevokeReason)

	must.NoError(t, et.s.RPC(structs.SecretEngineDeleteRPCMethod, del, &delResp))
	e, err := store.SecretEngineByName(nil, structs.DefaultNamespace, "nats")
	must.NoError(t, err)
	must.Nil(t, e)
	must.Nil(t, et.lease(created.Lease.ID))
}

// TestSecretLease_Lifecycle creates a credential for a task, renews it up to
// its max_ttl, and checks the revocations of the lease watcher: an expired
// lease, a stopped allocation and max_ttl; the NATS server refuses the
// revoked credentials.
func TestSecretLease_Lifecycle(t *testing.T) {
	ci.Parallel(t)
	et := newEngineTest(t, nil)
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
	other := &structs.SecretLeaseCreateRequest{Engine: "nats", Role: "admin", SecretName: "nats",
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: wi}}
	var otherResp structs.SecretLeaseCreateResponse
	must.ErrorContains(t, et.s.RPC(structs.SecretLeaseCreateRPCMethod, other, &otherResp), "has no secret block")

	created, err := et.create(wi)
	must.NoError(t, err)
	lease := created.Lease
	must.Eq(t, alloc.ID, lease.AllocID)
	must.Eq(t, "web", lease.Task)
	must.Eq(t, "nats", lease.SecretName)
	must.Eq(t, "nats.creds", created.File)
	must.Eq(t, "creds", created.FileItem)
	must.Eq(t, created.Items[secretengine.NATSItemUser], lease.BackendRef)
	must.False(t, lease.Revoked)
	must.NoError(t, et.connect(created.Items))

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
	// pushes the revocation: the NATS server refuses the credential.
	w := testLeaseWatcher(et.s, func() time.Time { return time.Unix(0, renewed.ExpireTime).Add(time.Millisecond) })
	w.evaluate(context.Background(), memdb.NewWatchSet())
	got := et.lease(lease.ID)
	must.True(t, got.Revoked)
	must.Eq(t, structs.SecretLeaseRevokeExpired, got.RevokeReason)
	must.Error(t, et.connect(created.Items))
	_, err = renew(lease.ID)
	must.ErrorContains(t, err, "revoked")

	// A stopped allocation: its lease is revoked at once.
	second, err := et.create(wi)
	must.NoError(t, err)
	must.NoError(t, et.connect(second.Items))
	stopped := alloc.Copy()
	stopped.ClientStatus = structs.AllocClientStatusComplete
	index, _ := et.s.fsm.State().LatestIndex()
	must.NoError(t, et.s.fsm.State().UpdateAllocsFromClient(structs.MsgTypeTestSetup, index+1, []*structs.Allocation{stopped}))
	w.now = time.Now
	w.evaluate(context.Background(), memdb.NewWatchSet())
	must.Eq(t, structs.SecretLeaseRevokeStopped, et.lease(second.Lease.ID).RevokeReason)
	must.Error(t, et.connect(second.Items))
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
// revokes the lease that is not renewed and pushes the revocation (it pushes
// the account JWT of every engine at its start).
func TestSecretLease_LeaderChange(t *testing.T) {
	ci.Parallel(t)
	ns := natstest.Start(t)
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

	var w structs.SecretWriteResponse
	must.NoError(t, leader.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("nats/admin", "", ns.Credential()), &w))
	et := &engineTest{t: t, s: leader, nats: ns}
	engine := et.engine()
	engine.Roles[0].TTL, engine.Roles[0].MaxTTL = 3*time.Second, time.Hour
	_, err := et.upsert("", engine)
	must.NoError(t, err)

	alloc := mock.Alloc()
	alloc.ClientStatus = structs.AllocClientStatusRunning
	alloc.Job.TaskGroups[0].Tasks[0].Secrets = []*structs.TaskSecret{{Name: "nats", Engine: "nats", Role: "worker"}}
	for _, s := range servers {
		// The allocation is put into every server's state directly.
		index, _ := s.fsm.State().LatestIndex()
		must.NoError(t, s.fsm.State().UpsertAllocs(structs.MsgTypeTestSetup, index+1, []*structs.Allocation{alloc.Copy()}))
	}
	wi, _, err := leader.encrypter.SignClaims(alloc.ToTaskIdentityClaims(alloc.Job, "web"))
	must.NoError(t, err)
	created, err := et.create(wi)
	must.NoError(t, err)
	must.NoError(t, et.connect(created.Items))

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
	waitForSecretStore(t, "credential refused", func() bool { return et.connect(created.Items) != nil })
}

// TestSecretEngine_RotateRoot rotates the account signing key: leases of the
// old key keep working until they end, then the old key is retired and
// removed from the account.
func TestSecretEngine_RotateRoot(t *testing.T) {
	ci.Parallel(t)
	et := newEngineTest(t, nil)
	_, err := et.upsert(et.root.SecretID, et.engine())
	must.NoError(t, err)
	_, wi := et.alloc(true)
	old, err := et.create(wi)
	must.NoError(t, err)

	var resp structs.SecretEngineWriteResponse
	must.NoError(t, et.s.RPC(structs.SecretEngineRotateRootRPCMethod, &structs.SecretEngineRotateRootRequest{Name: "nats",
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: et.root.SecretID}}, &resp))
	must.NotEq(t, old.Lease.SigningKey, resp.Key)
	must.Eq(t, []string{old.Lease.SigningKey}, resp.Engine.State.RetiredKeys)
	must.Positive(t, resp.Engine.State.RootRotateTime)
	sec, err := et.s.fsm.State().SecretByPath(nil, structs.DefaultNamespace, "nats/admin")
	must.NoError(t, err)
	must.Eq(t, 2, sec.CurrentVersion)

	fresh, err := et.create(wi)
	must.NoError(t, err)
	must.Eq(t, resp.Key, fresh.Lease.SigningKey)
	must.NoError(t, et.connect(old.Items))
	must.NoError(t, et.connect(fresh.Items))

	// While the old lease lives the key stays; once it is revoked the
	// watcher retires the key and removes it from the account.
	w := testLeaseWatcher(et.s, time.Now)
	w.evaluate(context.Background(), memdb.NewWatchSet())
	e, _ := et.s.fsm.State().SecretEngineByName(nil, structs.DefaultNamespace, "nats")
	must.Eq(t, []string{old.Lease.SigningKey}, e.State.RetiredKeys)

	var rev structs.SecretLeaseRevokeResponse
	must.NoError(t, et.s.RPC(structs.SecretLeaseRevokeRPCMethod, &structs.SecretLeaseRevokeRequest{LeaseID: old.Lease.ID,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: wi}}, &rev))
	must.Eq(t, structs.SecretLeaseRevokeStopped, et.lease(old.Lease.ID).RevokeReason)
	w.evaluate(context.Background(), memdb.NewWatchSet())
	e, _ = et.s.fsm.State().SecretEngineByName(nil, structs.DefaultNamespace, "nats")
	must.SliceEmpty(t, e.State.RetiredKeys)
	must.Eq(t, []string{old.Lease.SigningKey}, e.State.RemovedKeys)
	must.Error(t, et.connect(old.Items))
	must.NoError(t, et.connect(fresh.Items))
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
		&structs.SecretLeaseListRequest{Engine: "nats", QueryOptions: secretQuery(et.root.SecretID)}, &list))
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
	must.Eq(t, "engines/nats/creds/worker", create.Path)
	must.Eq(t, created.Lease.ID, create.LeaseID)
	must.Eq(t, "nats", create.Engine)
	must.Eq(t, "worker", create.Role)
	must.Eq(t, "web", create.Accessor.Task)

	outputs = append(outputs, list, rr, rev, created.Lease)
	canaries := []string{created.Items[secretengine.NATSItemSeed], created.Items[secretengine.NATSItemJWT],
		et.nats.AccountSigningSeed, et.nats.OperatorSigningSeed}
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
	assertSecretStoreCiphertext(t, et.s, et.nats.AccountSigningSeed)
	must.True(t, strings.Contains(strings.Join(types, ","), structs.TypeSecretLeaseCreated), must.Sprint(types))
	_ = fmt.Sprint
}

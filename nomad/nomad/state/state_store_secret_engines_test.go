// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"testing"
	"time"

	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func testSecretEngine(name string) *structs.SecretEngine {
	return &structs.SecretEngine{
		Namespace:      structs.DefaultNamespace,
		Name:           name,
		Type:           structs.SecretEngineTypeNATS,
		CredentialPath: "nats/admin",
		NATS:           &structs.SecretEngineNATS{Servers: []string{"nats://127.0.0.1:4222"}, AccountJWT: "jwt"},
		Roles:          []*structs.SecretEngineRole{{Name: "worker", TTL: time.Hour, MaxTTL: 2 * time.Hour}},
	}
}

func testSecretLease(id, engine string) *structs.SecretLease {
	now := time.Now()
	return &structs.SecretLease{
		ID: id, Namespace: structs.DefaultNamespace, Engine: engine, Role: "worker",
		IssueTime: now.UnixNano(), ExpireTime: now.Add(time.Hour).UnixNano(),
		MaxExpireTime: now.Add(2 * time.Hour).UnixNano(), BackendRef: "U" + id,
	}
}

func TestStateStore_SecretEngines(t *testing.T) {
	ci.Parallel(t)
	s := testStateStore(t)

	// Create: the expected index must be 0.
	must.ErrorIs(t, s.ApplySecretEngine(structs.MsgTypeTestSetup, 10,
		&structs.SecretEngineApplyRequest{Engine: testSecretEngine("nats"), ExpectedIndex: 5}), structs.ErrSecretConflict)
	must.NoError(t, s.ApplySecretEngine(structs.MsgTypeTestSetup, 11,
		&structs.SecretEngineApplyRequest{Engine: testSecretEngine("nats")}))
	must.NoError(t, s.ApplySecretEngine(structs.MsgTypeTestSetup, 12,
		&structs.SecretEngineApplyRequest{Engine: testSecretEngine("other")}))

	// Update with the current index.
	e := testSecretEngine("nats")
	e.Description = "updated"
	must.ErrorIs(t, s.ApplySecretEngine(structs.MsgTypeTestSetup, 13,
		&structs.SecretEngineApplyRequest{Engine: e, ExpectedIndex: 0}), structs.ErrSecretConflict)
	must.NoError(t, s.ApplySecretEngine(structs.MsgTypeTestSetup, 14,
		&structs.SecretEngineApplyRequest{Engine: e, ExpectedIndex: 11}))
	got, err := s.SecretEngineByName(nil, structs.DefaultNamespace, "nats")
	must.NoError(t, err)
	must.Eq(t, "updated", got.Description)
	must.Eq(t, 11, got.CreateIndex)
	must.Eq(t, 14, got.ModifyIndex)

	iter, err := s.SecretEnginesByNamespacePrefix(memdb.NewWatchSet(), structs.DefaultNamespace, "na")
	must.NoError(t, err)
	n := 0
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		n++
	}
	must.Eq(t, 1, n)

	// Leases need their engine.
	must.ErrorIs(t, s.ApplySecretLeases(structs.MsgTypeTestSetup, 20,
		&structs.SecretLeaseApplyRequest{Leases: []*structs.SecretLease{testSecretLease("l0", "missing")}}),
		structs.ErrSecretEngineNotFound)
	must.NoError(t, s.ApplySecretLeases(structs.MsgTypeTestSetup, 21, &structs.SecretLeaseApplyRequest{
		Leases: []*structs.SecretLease{testSecretLease("l1", "nats"), testSecretLease("l2", "nats"), testSecretLease("l3", "other")}}))

	// Revocation is final: a renewal computed before it is refused.
	l1, err := s.SecretLeaseByID(nil, "l1")
	must.NoError(t, err)
	revoked := l1.Copy()
	revoked.Revoked, revoked.RevokeTime, revoked.RevokeReason = true, 100, structs.SecretLeaseRevokeExpired
	must.NoError(t, s.ApplySecretLeases(structs.MsgTypeTestSetup, 22, &structs.SecretLeaseApplyRequest{Leases: []*structs.SecretLease{revoked}}))
	renewed := l1.Copy()
	renewed.Renewals++
	must.ErrorIs(t, s.ApplySecretLeases(structs.MsgTypeTestSetup, 23, &structs.SecretLeaseApplyRequest{Leases: []*structs.SecretLease{renewed}}),
		structs.ErrSecretLeaseRevoked)
	again := revoked.Copy()
	again.RevokeTime, again.RevokeReason = 200, structs.SecretLeaseRevokeOperator
	must.NoError(t, s.ApplySecretLeases(structs.MsgTypeTestSetup, 24, &structs.SecretLeaseApplyRequest{Leases: []*structs.SecretLease{again}}))
	l1, _ = s.SecretLeaseByID(nil, "l1")
	must.Eq(t, 100, l1.RevokeTime)
	must.Eq(t, structs.SecretLeaseRevokeExpired, l1.RevokeReason)
	must.Eq(t, 21, l1.CreateIndex)

	iter, err = s.SecretLeasesByEngine(nil, structs.DefaultNamespace, "nats")
	must.NoError(t, err)
	n = 0
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		n++
	}
	must.Eq(t, 2, n)

	// Purge of leases, and of an engine with its leases.
	must.NoError(t, s.PurgeSecretLeases(structs.MsgTypeTestSetup, 30, &structs.SecretLeasePurgeRequest{IDs: []string{"l2", "unknown"}}))
	l2, _ := s.SecretLeaseByID(nil, "l2")
	must.Nil(t, l2)
	must.ErrorIs(t, s.PurgeSecretEngine(structs.MsgTypeTestSetup, 31,
		&structs.SecretEnginePurgeRequest{Namespace: structs.DefaultNamespace, Name: "nats", ExpectedIndex: 11}), structs.ErrSecretConflict)
	must.NoError(t, s.PurgeSecretEngine(structs.MsgTypeTestSetup, 32,
		&structs.SecretEnginePurgeRequest{Namespace: structs.DefaultNamespace, Name: "nats", ExpectedIndex: 14}))
	got, _ = s.SecretEngineByName(nil, structs.DefaultNamespace, "nats")
	must.Nil(t, got)
	l1, _ = s.SecretLeaseByID(nil, "l1")
	must.Nil(t, l1)
	l3, _ := s.SecretLeaseByID(nil, "l3")
	must.NotNil(t, l3)
	idx, err := s.Index(TableSecretLeases)
	must.NoError(t, err)
	must.Eq(t, 32, idx)
}

func TestStateStore_SecretEngineEvents(t *testing.T) {
	ci.Parallel(t)
	e := testSecretEngine("nats")
	ev, ok := secretEngineEventFromChange(memdb.Change{Table: TableSecretEngines, After: e})
	must.True(t, ok)
	must.Eq(t, structs.TypeSecretEngineUpserted, ev.Type)
	must.Eq(t, "nats", ev.Key)

	l := testSecretLease("l1", "nats")
	ev, ok = secretEngineEventFromChange(memdb.Change{Table: TableSecretLeases, After: l})
	must.True(t, ok)
	must.Eq(t, structs.TypeSecretLeaseCreated, ev.Type)
	must.Eq(t, []string{"nats"}, ev.FilterKeys)

	renewed := l.Copy()
	renewed.Renewals = 1
	ev, _ = secretEngineEventFromChange(memdb.Change{Table: TableSecretLeases, Before: l, After: renewed})
	must.Eq(t, structs.TypeSecretLeaseRenewed, ev.Type)

	revoked := l.Copy()
	revoked.Revoked = true
	ev, _ = secretEngineEventFromChange(memdb.Change{Table: TableSecretLeases, Before: l, After: revoked})
	must.Eq(t, structs.TypeSecretLeaseRevoked, ev.Type)

	ev, _ = secretEngineEventFromChange(memdb.Change{Table: TableSecretLeases, Before: revoked})
	must.Eq(t, structs.TypeSecretLeaseDeleted, ev.Type)
}

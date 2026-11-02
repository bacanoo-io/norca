// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"
	"time"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// TestSecretEngineFSM_SnapshotRestore applies an engine and a lease through
// the FSM and restores them from a snapshot.
func TestSecretEngineFSM_SnapshotRestore(t *testing.T) {
	ci.Parallel(t)
	fsm := testFSM(t)
	engine := &structs.SecretEngine{Namespace: "default", Name: "nats", Type: structs.SecretEngineTypeOperator,
		Operator: &structs.SecretEngineOperator{Service: "pg"},
		Roles:    []*structs.SecretEngineRole{{Name: "worker", TTL: time.Hour, MaxTTL: 2 * time.Hour}}}
	apply := func(mt structs.MessageType, req any) any {
		buf, err := structs.Encode(mt, req)
		must.NoError(t, err)
		return fsm.Apply(makeLog(buf))
	}
	must.Nil(t, apply(structs.SecretEngineApplyRequestType, &structs.SecretEngineApplyRequest{Engine: engine}))
	lease := &structs.SecretLease{ID: "l1", Namespace: "default", Engine: "nats", Role: "worker",
		ExpireTime: time.Now().Add(time.Hour).UnixNano(), BackendRef: "UABC"}
	must.Nil(t, apply(structs.SecretLeaseApplyRequestType, &structs.SecretLeaseApplyRequest{Leases: []*structs.SecretLease{lease}}))

	// A lease of an unknown engine is refused by the FSM.
	orphan := lease.Copy()
	orphan.ID, orphan.Engine = "l2", "missing"
	res := apply(structs.SecretLeaseApplyRequestType, &structs.SecretLeaseApplyRequest{Leases: []*structs.SecretLease{orphan}})
	must.ErrorIs(t, res.(error), structs.ErrSecretEngineNotFound)

	fsm2 := testSnapshotRestore(t, fsm)
	e, err := fsm2.State().SecretEngineByName(nil, "default", "nats")
	must.NoError(t, err)
	must.Eq(t, "pg", e.Operator.Service)
	l, err := fsm2.State().SecretLeaseByID(nil, "l1")
	must.NoError(t, err)
	must.Eq(t, "UABC", l.BackendRef)

	// The purge of the engine removes its leases.
	fsm = fsm2
	must.Nil(t, apply(structs.SecretEnginePurgeRequestType,
		&structs.SecretEnginePurgeRequest{Namespace: "default", Name: "nats", ExpectedIndex: e.ModifyIndex}))
	l, _ = fsm.State().SecretLeaseByID(nil, "l1")
	must.Nil(t, l)

	must.Nil(t, apply(structs.SecretLeasePurgeRequestType, &structs.SecretLeasePurgeRequest{IDs: []string{"l1"}}))
}

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

func testStateAdmissionPolicy(name string) *structs.AdmissionPolicy {
	return &structs.AdmissionPolicy{
		Name:   name,
		Action: structs.AdmissionPolicyActionDeny,
		Rules:  []*structs.AdmissionPolicyRule{{Expression: "true"}},
	}
}

func TestStateStore_AdmissionPolicies(t *testing.T) {
	ci.Parallel(t)
	store := testStateStore(t)

	ws := memdb.NewWatchSet()
	_, err := store.AdmissionPolicyByName(ws, "a")
	must.NoError(t, err)

	must.NoError(t, store.UpsertAdmissionPolicies(structs.MsgTypeTestSetup, 10,
		[]*structs.AdmissionPolicy{testStateAdmissionPolicy("a"), testStateAdmissionPolicy("ab"), testStateAdmissionPolicy("b")}))
	must.True(t, watchFired(ws))

	p, err := store.AdmissionPolicyByName(nil, "a")
	must.NoError(t, err)
	must.Eq(t, uint64(10), p.CreateIndex)
	must.Eq(t, uint64(10), p.ModifyIndex)

	// Update keeps the create index.
	must.NoError(t, store.UpsertAdmissionPolicies(structs.MsgTypeTestSetup, 11,
		[]*structs.AdmissionPolicy{testStateAdmissionPolicy("a")}))
	p, err = store.AdmissionPolicyByName(nil, "a")
	must.NoError(t, err)
	must.Eq(t, uint64(10), p.CreateIndex)
	must.Eq(t, uint64(11), p.ModifyIndex)

	idx, err := store.Index(TableAdmissionPolicies)
	must.NoError(t, err)
	must.Eq(t, uint64(11), idx)

	count := func(iter memdb.ResultIterator) int {
		n := 0
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			n++
		}
		return n
	}
	iter, err := store.AdmissionPolicies(nil, SortDefault)
	must.NoError(t, err)
	must.Eq(t, 3, count(iter))
	iter, err = store.AdmissionPoliciesByNamePrefix(nil, "a", SortReverse)
	must.NoError(t, err)
	must.Eq(t, 2, count(iter))

	// Delete.
	must.NoError(t, store.DeleteAdmissionPolicies(structs.MsgTypeTestSetup, 12, []string{"a", "b"}))
	iter, err = store.AdmissionPolicies(nil, SortDefault)
	must.NoError(t, err)
	must.Eq(t, 1, count(iter))

	must.ErrorContains(t, store.DeleteAdmissionPolicies(structs.MsgTypeTestSetup, 13, []string{"missing"}), "not found")
}

func TestStateStore_AdmissionPolicyEvents(t *testing.T) {
	ci.Parallel(t)
	store := TestStateStoreCfg(t, TestStateStorePublisher(t))
	defer store.StopEventBroker()

	must.NoError(t, store.UpsertAdmissionPolicies(structs.AdmissionPolicyUpsertRequestType, 10,
		[]*structs.AdmissionPolicy{testStateAdmissionPolicy("a")}))
	must.NoError(t, store.DeleteAdmissionPolicies(structs.AdmissionPolicyDeleteRequestType, 11, []string{"a"}))

	events := WaitForEvents(t, store, 10, 2, time.Second)
	must.Len(t, 2, events)
	must.Eq(t, structs.TopicAdmissionPolicy, events[0].Topic)
	must.Eq(t, structs.TypeAdmissionPolicyUpserted, events[0].Type)
	must.Eq(t, "a", events[0].Key)
	must.Eq(t, structs.TypeAdmissionPolicyDeleted, events[1].Type)
	must.Eq(t, "a", events[1].Payload.(*structs.AdmissionPolicyEvent).AdmissionPolicy.Name)
}

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

func testStateNetworkPolicy(namespace, name string, selector map[string]string) *structs.NetworkPolicy {
	return &structs.NetworkPolicy{
		Name:        name,
		Namespace:   namespace,
		Selector:    selector,
		Egress:      []*structs.NetworkPolicyRule{{Ports: []string{"443/tcp"}}},
		Enforcement: structs.NetworkPolicyEnforcementRequired,
	}
}

func TestStateStore_NetworkPolicies(t *testing.T) {
	ci.Parallel(t)
	store := testStateStore(t)

	ws := memdb.NewWatchSet()
	_, err := store.NetworkPolicyByName(ws, "prod", "a")
	must.NoError(t, err)

	must.NoError(t, store.UpsertNetworkPolicies(structs.MsgTypeTestSetup, 10, []*structs.NetworkPolicy{
		testStateNetworkPolicy("prod", "a", nil),
		testStateNetworkPolicy("prod", "b", map[string]string{"app": "db"}),
		testStateNetworkPolicy("dev", "a", nil),
	}))
	must.True(t, watchFired(ws))

	p, err := store.NetworkPolicyByName(nil, "prod", "a")
	must.NoError(t, err)
	must.Eq(t, uint64(10), p.CreateIndex)

	must.NoError(t, store.UpsertNetworkPolicies(structs.MsgTypeTestSetup, 11, []*structs.NetworkPolicy{
		testStateNetworkPolicy("prod", "a", map[string]string{"app": "web"}),
	}))
	p, err = store.NetworkPolicyByName(nil, "prod", "a")
	must.NoError(t, err)
	must.Eq(t, uint64(10), p.CreateIndex)
	must.Eq(t, uint64(11), p.ModifyIndex)

	count := func(iter memdb.ResultIterator) int {
		n := 0
		for raw := iter.Next(); raw != nil; raw = iter.Next() {
			n++
		}
		return n
	}
	iter, err := store.NetworkPoliciesByNamespace(nil, "prod", SortDefault)
	must.NoError(t, err)
	must.Eq(t, 2, count(iter))
	iter, err = store.NetworkPolicies(nil, SortDefault)
	must.NoError(t, err)
	must.Eq(t, 3, count(iter))

	// Selection by namespace and meta.
	policies, admin, err := store.NetworkPoliciesForGroup(nil, "prod", map[string]string{"app": "db"})
	must.NoError(t, err)
	must.Len(t, 1, policies)
	must.Eq(t, "b", policies[0].Name)
	must.Len(t, 0, admin)

	must.Error(t, store.DeleteNetworkPolicies(structs.MsgTypeTestSetup, 12, "prod", []string{"missing"}))
	must.NoError(t, store.DeleteNetworkPolicies(structs.MsgTypeTestSetup, 12, "prod", []string{"a"}))
	p, err = store.NetworkPolicyByName(nil, "prod", "a")
	must.NoError(t, err)
	must.Nil(t, p)
	p, err = store.NetworkPolicyByName(nil, "dev", "a")
	must.NoError(t, err)
	must.NotNil(t, p)

	idx, err := store.Index(TableNetworkPolicies)
	must.NoError(t, err)
	must.Eq(t, uint64(12), idx)
}

func TestStateStore_AdminNetworkPolicies(t *testing.T) {
	ci.Parallel(t)
	store := testStateStore(t)

	must.NoError(t, store.UpsertAdminNetworkPolicies(structs.MsgTypeTestSetup, 10, []*structs.AdminNetworkPolicy{
		{Name: "late", Priority: 100, Namespaces: []string{"prod"}},
		{Name: "early", Priority: 1},
		{Name: "web-only", Priority: 1, Selector: map[string]string{"tier": "web"}},
	}))

	_, admin, err := store.NetworkPoliciesForGroup(nil, "prod", map[string]string{"tier": "db"})
	must.NoError(t, err)
	must.Len(t, 2, admin)
	must.Eq(t, "early", admin[0].Name)
	must.Eq(t, "late", admin[1].Name)

	_, admin, err = store.NetworkPoliciesForGroup(nil, "dev", map[string]string{"tier": "web"})
	must.NoError(t, err)
	must.Len(t, 2, admin)
	must.Eq(t, "early", admin[0].Name)
	must.Eq(t, "web-only", admin[1].Name)

	must.Error(t, store.DeleteAdminNetworkPolicies(structs.MsgTypeTestSetup, 11, []string{"missing"}))
	must.NoError(t, store.DeleteAdminNetworkPolicies(structs.MsgTypeTestSetup, 11, []string{"early"}))
	p, err := store.AdminNetworkPolicyByName(nil, "early")
	must.NoError(t, err)
	must.Nil(t, p)
}

func TestStateStore_NetworkPolicyEvents(t *testing.T) {
	ci.Parallel(t)
	store := TestStateStoreCfg(t, TestStateStorePublisher(t))
	defer store.StopEventBroker()

	policy := testStateNetworkPolicy("default", "a", nil)
	must.NoError(t, store.UpsertNetworkPolicies(structs.NetworkPolicyUpsertRequestType, 10, []*structs.NetworkPolicy{policy}))
	must.NoError(t, store.UpsertAdminNetworkPolicies(structs.AdminNetworkPolicyUpsertRequestType, 11,
		[]*structs.AdminNetworkPolicy{{Name: "adm"}}))
	must.NoError(t, store.DeleteNetworkPolicies(structs.NetworkPolicyDeleteRequestType, 12, "default", []string{"a"}))

	events := WaitForEvents(t, store, 10, 3, 5*time.Second)
	must.Len(t, 3, events)
	must.Eq(t, structs.TopicNetworkPolicy, events[0].Topic)
	must.Eq(t, structs.TypeNetworkPolicyUpserted, events[0].Type)
	must.Eq(t, "default", events[0].Namespace)
	must.Eq(t, "a", events[0].Key)
	must.Eq(t, structs.TypeAdminNetworkPolicyUpserted, events[1].Type)
	_, ok := events[1].Payload.(*structs.AdminNetworkPolicyEvent)
	must.True(t, ok)
	must.Eq(t, "", events[1].Namespace)
	must.Eq(t, structs.TypeNetworkPolicyDeleted, events[2].Type)
}

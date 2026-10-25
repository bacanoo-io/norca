// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"testing"

	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestStateStore_MTLSRoots(t *testing.T) {
	ci.Parallel(t)
	store := testStateStore(t)

	r1 := &structs.MTLSRoot{ID: "r1", Region: "global", Active: true, SigningKeyID: "key1", SigningKey: []byte("x")}
	r2 := &structs.MTLSRoot{ID: "r2", Region: "other"}
	must.NoError(t, store.UpsertMTLSRoots(structs.MsgTypeTestSetup, 10, []*structs.MTLSRoot{r1, r2}, nil))

	roots, err := store.MTLSRoots(nil)
	must.NoError(t, err)
	must.Len(t, 2, roots)
	got, err := store.MTLSRootByID(nil, "r1")
	must.NoError(t, err)
	must.Eq(t, uint64(10), got.CreateIndex)

	// The root key that encrypts a CA key is in use.
	inUse, err := store.IsRootKeyMetaInUse("key1")
	must.NoError(t, err)
	must.True(t, inUse)
	inUse, err = store.IsRootKeyMetaInUse("key2")
	must.NoError(t, err)
	must.False(t, inUse)

	// Upsert and delete in one transaction.
	ws := memdb.NewWatchSet()
	_, err = store.MTLSRoots(ws)
	must.NoError(t, err)
	r1b := r1.Copy()
	r1b.SigningKeyID = "key2"
	must.NoError(t, store.UpsertMTLSRoots(structs.MsgTypeTestSetup, 11, []*structs.MTLSRoot{r1b}, []string{"r2", "missing"}))
	must.True(t, watchFired(ws))
	roots, err = store.MTLSRoots(nil)
	must.NoError(t, err)
	must.Len(t, 1, roots)
	must.Eq(t, uint64(10), roots[0].CreateIndex)
	must.Eq(t, uint64(11), roots[0].ModifyIndex)
	inUse, err = store.IsRootKeyMetaInUse("key1")
	must.NoError(t, err)
	must.False(t, inUse)
	index, err := store.Index(TableMTLSRoots)
	must.NoError(t, err)
	must.Eq(t, uint64(11), index)
}

func TestStateStore_ServiceIntentions(t *testing.T) {
	ci.Parallel(t)
	store := testStateStore(t)

	a := &structs.ServiceIntention{Namespace: "default", Destination: "api", SourceNamespace: "default", Source: "web", Action: "allow"}
	b := &structs.ServiceIntention{Namespace: "default", Destination: "api", SourceNamespace: "*", Source: "*", Action: "deny"}
	c := &structs.ServiceIntention{Namespace: "prod", Destination: "db", SourceNamespace: "prod", Source: "api", Action: "allow"}
	must.NoError(t, store.UpsertServiceIntentions(structs.MsgTypeTestSetup, 10, []*structs.ServiceIntention{a, b, c}))

	list, err := store.ServiceIntentions(nil, "default")
	must.NoError(t, err)
	must.Len(t, 2, list)
	list, err = store.ServiceIntentions(nil, structs.AllNamespacesSentinel)
	must.NoError(t, err)
	must.Len(t, 3, list)

	// Same key replaces.
	a2 := a.Copy()
	a2.Action = "deny"
	must.NoError(t, store.UpsertServiceIntentions(structs.MsgTypeTestSetup, 11, []*structs.ServiceIntention{a2}))
	got, err := store.ServiceIntentionByKey(nil, "default", "api", "default", "web")
	must.NoError(t, err)
	must.Eq(t, "deny", got.Action)
	must.Eq(t, uint64(10), got.CreateIndex)
	must.Eq(t, uint64(11), got.ModifyIndex)

	must.NoError(t, store.DeleteServiceIntentions(structs.MsgTypeTestSetup, 12, []*structs.ServiceIntention{a, c}))
	list, err = store.ServiceIntentions(nil, structs.AllNamespacesSentinel)
	must.NoError(t, err)
	must.Len(t, 1, list)
	must.Eq(t, "*", list[0].Source)
}

func TestEventsFromChanges_ServiceIntention(t *testing.T) {
	ci.Parallel(t)
	s := TestStateStoreCfg(t, TestStateStorePublisher(t))
	defer s.StopEventBroker()

	intention := &structs.ServiceIntention{Namespace: "prod", Destination: "api", SourceNamespace: "prod", Source: "web", Action: "allow"}
	out := eventsFromChanges(s.db.ReadTxn(), Changes{
		Index:   100,
		MsgType: structs.ServiceIntentionUpsertRequestType,
		Changes: memdb.Changes{{Table: TableServiceIntentions, After: intention}},
	})
	must.NotNil(t, out)
	must.Len(t, 1, out.Events)
	e := out.Events[0]
	must.Eq(t, structs.TopicServiceIntention, e.Topic)
	must.Eq(t, structs.TypeServiceIntentionUpserted, e.Type)
	must.Eq(t, "api", e.Key)
	must.Eq(t, "prod", e.Namespace)
	must.Eq(t, []string{"web"}, e.FilterKeys)

	out = eventsFromChanges(s.db.ReadTxn(), Changes{
		Index:   101,
		MsgType: structs.ServiceIntentionDeleteRequestType,
		Changes: memdb.Changes{{Table: TableServiceIntentions, Before: intention}},
	})
	must.Len(t, 1, out.Events)
	must.Eq(t, structs.TypeServiceIntentionDeleted, out.Events[0].Type)

	// Trust bundle changes are not published.
	out = eventsFromChanges(s.db.ReadTxn(), Changes{
		Index:   102,
		MsgType: structs.MTLSRootsUpsertRequestType,
		Changes: memdb.Changes{{Table: TableMTLSRoots, After: &structs.MTLSRoot{ID: "r"}}},
	})
	must.Nil(t, out)
}

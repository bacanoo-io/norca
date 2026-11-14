// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"
)

// TestBusEvents_HalfBatch checks what the publisher finds and does when the
// stream ends in the middle of the events of one Raft index (a batch whose
// acknowledgements were lost, a leader change): it continues with that
// index and publishes the events of it the stream does not have. It used to
// continue after the index, and the rest of the batch was never published.
func TestBusEvents_HalfBatch(t *testing.T) {
	ci.Parallel(t)

	s1, cleanup := TestServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	js := busJS(t, s1)
	ctx := t.Context()

	// The stream of the server's publisher is left alone: the test keeps
	// one of its own with the same headers.
	_, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "HALF", Subjects: []string{"halfbatch.>"}})
	must.NoError(t, err)
	stream, err := js.Stream(ctx, "HALF")
	must.NoError(t, err)

	_, _, found, err := bus.LastEvents(ctx, stream)
	must.NoError(t, err)
	must.False(t, found)

	event := func(index uint64, key string) *bus.Event {
		return &bus.Event{Index: index, Topic: "Job", Type: "JobRegistered", Namespace: "default", Key: key, JSON: []byte(`{}`)}
	}
	publish := func(ev *bus.Event) {
		_, err := js.PublishMsg(ctx, bus.EventMessage("halfbatch", "global", ev))
		must.NoError(t, err)
	}
	// Index 10 complete, a gap marker, then two of the three events of 11.
	publish(event(10, "a"))
	publish(event(10, "b"))
	_, err = js.PublishMsg(ctx, bus.GapMessage("halfbatch", "global", 11, "test", time.Now()))
	must.NoError(t, err)
	publish(event(11, "a"))
	publish(event(11, "b"))

	index, stored, found, err := bus.LastEvents(ctx, stream)
	must.NoError(t, err)
	must.True(t, found)
	must.Eq(t, 11, index)
	must.MapLen(t, 2, stored)
	must.True(t, stored[bus.EventMsgID("global", event(11, "a"))])
	must.True(t, stored[bus.EventMsgID("global", event(11, "b"))])
	must.False(t, stored[bus.EventMsgID("global", event(10, "a"))])

	// The batch of index 11 again, as the broker gives it: only the third
	// event is published.
	batch := &structs.Events{Index: 11, Events: []structs.Event{
		{Index: 11, Topic: "Job", Type: "JobRegistered", Namespace: "default", Key: "a"},
		{Index: 11, Topic: "Job", Type: "JobRegistered", Namespace: "default", Key: "b"},
		{Index: 11, Topic: "Job", Type: "JobRegistered", Namespace: "default", Key: "c"},
	}}
	msgs := s1.busEventMessages("halfbatch", batch, stored)
	must.Len(t, 1, msgs)
	must.Eq(t, bus.EventMsgID("global", event(11, "c")), msgs[0].Header.Get(bus.HeaderMsgID))
	must.Len(t, 3, s1.busEventMessages("halfbatch", batch, nil))

	// The index helper of before still names the newest index.
	last, found, err := bus.LastEventIndex(ctx, stream)
	must.NoError(t, err)
	must.True(t, found)
	must.Eq(t, 11, last)
}

// TestBusEvents_PushNoticeCooldown checks a push notice that comes while its
// registry is in the cooldown of a sync: it is not dropped, it waits for the
// end of the cooldown and is synced then. A notice pushed before the last
// sync started has been synced and waits for nothing.
func TestBusEvents_PushNoticeCooldown(t *testing.T) {
	ci.Parallel(t)

	s1, cleanup := TestServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	var reg PackRegistryResponse
	must.NoError(t, msgpackrpc.CallWithCodec(rpcClient(t, s1), "Packs.Upsert", &PackRegistryUpsertRequest{
		Registry:     &PackRegistry{Name: "local", Spec: &PackRegistrySpec{Type: "dir", Source: "builtin"}},
		WriteRequest: structs.WriteRequest{Region: "global"},
	}, &reg))

	syncer := &pushSyncer{srv: s1, last: map[string]time.Time{}}
	notice := &bus.PushNotice{Registry: "local"}
	start := time.Now()

	// The first notice syncs.
	wait, err := syncer.handle(notice, start)
	must.NoError(t, err)
	must.Eq(t, 0, wait)
	synced := syncer.last["local"]
	must.False(t, synced.IsZero())

	// A push after that sync, within the cooldown: to be handled again
	// when the cooldown is over, and not synced now.
	wait, err = syncer.handle(notice, time.Now().Add(time.Millisecond))
	must.NoError(t, err)
	must.Positive(t, wait)
	must.LessEq(t, pushSyncCooldown, wait)
	must.Eq(t, synced, syncer.last["local"])

	// A notice pushed before the sync started is covered by it.
	wait, err = syncer.handle(notice, synced.Add(-time.Second))
	must.NoError(t, err)
	must.Eq(t, 0, wait)
	must.Eq(t, synced, syncer.last["local"])

	// After the cooldown the later push is synced.
	syncer.last["local"] = synced.Add(-pushSyncCooldown - time.Second)
	wait, err = syncer.handle(notice, time.Now())
	must.NoError(t, err)
	must.Eq(t, 0, wait)
	must.True(t, syncer.last["local"].After(synced.Add(-time.Second)))
}

// TestBus_CheckSubjects checks the region names and subject prefixes whose
// streams would share subjects.
func TestBus_CheckSubjects(t *testing.T) {
	ci.Parallel(t)

	for _, ok := range [][2]string{
		{"norca", "global"}, {"norca", "eu-west"}, {"events", "telemetry"}, {"norca", "packs-eu"},
	} {
		must.NoError(t, bus.CheckSubjects(ok[0], ok[1]), must.Sprint(ok))
	}
	for _, bad := range [][2]string{
		{"norca", "telemetry"},        // norca.telemetry.> and norca.telemetry.telemetry.*
		{"norca", "packs"},            // norca.packs.> and norca.packs.pushed
		{"events", "packs"},           // events.packs.> and events.packs.pushed
		{"norca.telemetry", "global"}, // norca.telemetry.global.> and norca.telemetry.global.*
	} {
		must.Error(t, bus.CheckSubjects(bad[0], bad[1]), must.Sprint(bad))
	}

	// A server with such a region name does not start its bus.
	_, stop, err := TestServerErr(t, func(c *Config) {
		enableBus(t)(c)
		c.Region = "packs"
	})
	if stop != nil {
		defer stop()
	}
	must.ErrorContains(t, err, "would share subjects")
}

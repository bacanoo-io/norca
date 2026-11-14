// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"
)

// TestCDCEndpoint_Consumers checks the consumers the servers keep for the
// readers of the change streams: made by the endpoint after the checks of
// the filters, one per reader and stream within max_consumers, read by the
// reader on the bus in the stream's order, out of reach of other readers.
func TestCDCEndpoint_Consumers(t *testing.T) {
	ci.Parallel(t)

	s1, _, cleanup := TestACLServer(t, func(c *Config) {
		enableBus(t)(c)
		c.Bus.Events.MaxConsumers = 2
	})
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	st := s1.fsm.State()
	codec := rpcClient(t, s1)

	policy := mock.ACLPolicy()
	policy.Name = "cdc-reader"
	policy.Rules = `cdc { receive = ["norca.global.default.>"] }`
	policy.SetHash()
	plain := mock.ACLPolicy()
	must.NoError(t, st.UpsertACLPolicies(structs.MsgTypeTestSetup, 100, []*structs.ACLPolicy{policy, plain}))
	a, b, c, noCDC := mock.ACLToken(), mock.ACLToken(), mock.ACLToken(), mock.ACLToken()
	for _, tok := range []*structs.ACLToken{a, b, c} {
		tok.Policies = []string{policy.Name}
		tok.SetHash()
	}
	noCDC.Policies = []string{plain.Name}
	noCDC.SetHash()
	must.NoError(t, st.UpsertACLTokens(structs.MsgTypeTestSetup, 101, []*structs.ACLToken{a, b, c, noCDC}))

	js := busJS(t, s1)
	waitBus(t, func() (bool, error) {
		_, err := js.Stream(t.Context(), bus.EventStreamName("global"))
		return err == nil, err
	})

	upsert := func(token *structs.ACLToken, stream string, filters ...string) (*structs.CDCConsumer, error) {
		var resp structs.CDCConsumerWriteResponse
		err := msgpackrpc.CallWithCodec(codec, structs.CDCConsumerUpsertRPCMethod, &structs.CDCConsumerUpsertRequest{
			Stream:       stream,
			Consumer:     structs.CDCConsumerSpec{Filters: filters},
			WriteRequest: structs.WriteRequest{Region: "global", AuthToken: token.SecretID},
		}, &resp)
		return resp.Consumer, err
	}
	refused := func(err error, code int) {
		t.Helper()
		got, _, ok := structs.CodeFromRPCCodedErr(err)
		must.True(t, ok, must.Sprintf("not a coded error: %v", err))
		must.Eq(t, code, got, must.Sprint(err))
	}

	// The reader's consumer, named after its token, with two filters.
	consumer, err := upsert(a, structs.CDCStreamEvents, "norca.global.default.Evaluation", "norca.global.default.Allocation")
	must.NoError(t, err)
	must.Eq(t, a.AccessorID, consumer.Name)
	must.Eq(t, bus.EventStreamName("global"), consumer.StreamName)
	must.Eq(t, []string{"norca.global.default.Evaluation", "norca.global.default.Allocation"}, consumer.Filters)

	// The checks.
	_, err = upsert(a, structs.CDCStreamEvents, "norca.global.other.Job")
	refused(err, http.StatusForbidden)
	_, err = upsert(a, structs.CDCStreamEvents, "norca.global.default.Job", "norca.global.default.*")
	refused(err, http.StatusBadRequest)
	_, err = upsert(a, structs.CDCStreamEvents, "norca.global.default.Job.extra")
	refused(err, http.StatusBadRequest)
	_, err = upsert(a, structs.CDCStreamAdmin, "norca.admin.global._.ACLPolicy")
	refused(err, http.StatusForbidden)
	_, err = upsert(a, "audit")
	refused(err, http.StatusBadRequest)
	_, err = upsert(noCDC, structs.CDCStreamEvents, "norca.global.default.Job")
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())

	// One consumer per reader: a second reader fits, a third does not.
	_, err = upsert(b, structs.CDCStreamEvents, "norca.global.default.Job")
	must.NoError(t, err)
	_, err = upsert(c, structs.CDCStreamEvents, "norca.global.default.Job")
	refused(err, http.StatusConflict)

	// The reader reads its consumer on the bus, in the stream's order.
	nc, err := nats.Connect(busURL(s1), nats.Token(a.SecretID), nats.CustomInboxPrefix(bus.InboxPrefix+"."+a.AccessorID))
	must.NoError(t, err)
	defer nc.Close()
	readerJS, err := jetstream.New(nc)
	must.NoError(t, err)
	cons, err := readerJS.Consumer(t.Context(), bus.EventStreamName("global"), a.AccessorID)
	must.NoError(t, err)

	eval := mock.Eval()
	must.NoError(t, st.UpsertEvals(structs.EvalUpdateRequestType, 110, []*structs.Evaluation{eval}))
	must.NoError(t, st.UpsertJob(structs.JobRegisterRequestType, 111, nil, mock.Job()))
	alloc := mock.Alloc()
	must.NoError(t, st.UpsertAllocs(structs.MsgTypeTestSetup, 112, []*structs.Allocation{alloc}))
	running := alloc.Copy()
	running.ClientStatus = structs.AllocClientStatusRunning
	must.NoError(t, st.UpdateAllocsFromClient(structs.AllocClientUpdateRequestType, 113, []*structs.Allocation{running}))

	var topics []string
	deadline := time.Now().Add(20 * time.Second)
	for len(topics) < 2 && time.Now().Before(deadline) {
		batch, err := cons.Fetch(10, jetstream.FetchMaxWait(time.Second))
		must.NoError(t, err)
		for msg := range batch.Messages() {
			topics = append(topics, msg.Headers().Get(bus.HeaderTopic))
			must.NoError(t, msg.Ack())
		}
	}
	must.Eq(t, []string{"Evaluation", "Allocation"}, topics)

	// Another reader's consumer is out of reach.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err = readerJS.Consumer(ctx, bus.EventStreamName("global"), b.AccessorID)
	must.Error(t, err)

	// Read and delete through the endpoint.
	var read structs.CDCConsumerResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.CDCConsumerReadRPCMethod, &structs.CDCConsumerReadRequest{
		Stream:       structs.CDCStreamEvents,
		QueryOptions: structs.QueryOptions{Region: "global", AuthToken: a.SecretID},
	}, &read))
	must.NotNil(t, read.Consumer)
	must.Eq(t, uint64(0), read.Consumer.NumPending)
	var del structs.CDCConsumerWriteResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.CDCConsumerDeleteRPCMethod, &structs.CDCConsumerDeleteRequest{
		Stream:       structs.CDCStreamEvents,
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: a.SecretID},
	}, &del))
	read = structs.CDCConsumerResponse{}
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.CDCConsumerReadRPCMethod, &structs.CDCConsumerReadRequest{
		Stream:       structs.CDCStreamEvents,
		QueryOptions: structs.QueryOptions{Region: "global", AuthToken: a.SecretID},
	}, &read))
	must.Nil(t, read.Consumer)
}

// TestCDCConsumers_Cleanup checks that the leader removes the consumer of a
// reader that lost its right: a deleted token, and a policy that no longer
// has the cdc rule.
func TestCDCConsumers_Cleanup(t *testing.T) {
	ci.Parallel(t)

	s1, _, cleanup := TestACLServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	st := s1.fsm.State()
	codec := rpcClient(t, s1)

	keep, change := mock.ACLPolicy(), mock.ACLPolicy()
	keep.Name, change.Name = "cdc-keep", "cdc-change"
	keep.Rules = `cdc { receive = ["norca.global.default.>"] }`
	change.Rules = keep.Rules
	keep.SetHash()
	change.SetHash()
	must.NoError(t, st.UpsertACLPolicies(structs.MsgTypeTestSetup, 100, []*structs.ACLPolicy{keep, change}))
	deleted, changed := mock.ACLToken(), mock.ACLToken()
	deleted.Policies, changed.Policies = []string{keep.Name}, []string{change.Name}
	deleted.SetHash()
	changed.SetHash()
	must.NoError(t, st.UpsertACLTokens(structs.MsgTypeTestSetup, 101, []*structs.ACLToken{deleted, changed}))

	js := busJS(t, s1)
	waitBus(t, func() (bool, error) {
		_, err := js.Stream(t.Context(), bus.EventStreamName("global"))
		return err == nil, err
	})
	for _, token := range []*structs.ACLToken{deleted, changed} {
		var resp structs.CDCConsumerWriteResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.CDCConsumerUpsertRPCMethod, &structs.CDCConsumerUpsertRequest{
			Stream:       structs.CDCStreamEvents,
			Consumer:     structs.CDCConsumerSpec{Filters: []string{"norca.global.default.Job"}},
			WriteRequest: structs.WriteRequest{Region: "global", AuthToken: token.SecretID},
		}, &resp))
	}
	gone := func(name string) bool {
		_, err := js.Consumer(t.Context(), bus.EventStreamName("global"), name)
		return err != nil
	}
	must.False(t, gone(deleted.AccessorID))
	must.False(t, gone(changed.AccessorID))

	must.NoError(t, st.DeleteACLTokens(structs.MsgTypeTestSetup, 110, []string{deleted.AccessorID}))
	changedPolicy := *change
	change = &changedPolicy
	change.Rules = `namespace "default" { policy = "read" }`
	change.SetHash()
	must.NoError(t, st.UpsertACLPolicies(structs.MsgTypeTestSetup, 111, []*structs.ACLPolicy{change}))

	waitBus(t, func() (bool, error) {
		return gone(deleted.AccessorID) && gone(changed.AccessorID), nil
	})
}

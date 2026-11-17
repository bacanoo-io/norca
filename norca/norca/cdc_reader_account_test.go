// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
)

// cdcTestTokens writes policies and one token per policy into the state.
func cdcTestTokens(t *testing.T, s *Server, rules map[string]string) map[string]*structs.ACLToken {
	t.Helper()
	st := s.fsm.State()
	index, err := st.LatestIndex()
	must.NoError(t, err)
	out := map[string]*structs.ACLToken{}
	for name, rule := range rules {
		policy := mock.ACLPolicy()
		policy.Name = name
		policy.Rules = rule
		policy.SetHash()
		index++
		must.NoError(t, st.UpsertACLPolicies(structs.MsgTypeTestSetup, index, []*structs.ACLPolicy{policy}))
		token := mock.ACLToken()
		token.Name = name
		token.Policies = []string{name}
		token.SetHash()
		index++
		must.NoError(t, st.UpsertACLTokens(structs.MsgTypeTestSetup, index, []*structs.ACLToken{token}))
		out[name] = token
	}
	return out
}

// cdcTestConsumer makes the consumer of a token for the events of the
// default namespace.
func cdcTestConsumer(t *testing.T, s *Server, token *structs.ACLToken, filters ...string) {
	t.Helper()
	codec := rpcClient(t, s)
	defer codec.Close()
	var resp structs.CDCConsumerWriteResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.CDCConsumerUpsertRPCMethod, &structs.CDCConsumerUpsertRequest{
		Stream:       structs.CDCStreamEvents,
		Consumer:     structs.CDCConsumerSpec{Filters: filters},
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: token.SecretID},
	}, &resp))
}

// cdcTestBus waits for the streams and the Consul KV bucket of the bus and
// returns the JetStream of the servers' own connection with the bucket.
func cdcTestBus(t *testing.T, s *Server) (jetstream.JetStream, jetstream.KeyValue) {
	t.Helper()
	js := busJS(t, s)
	var kv jetstream.KeyValue
	waitBus(t, func() (bool, error) {
		if _, err := js.Stream(t.Context(), bus.EventStreamName("global")); err != nil {
			return false, err
		}
		var err error
		kv, err = js.KeyValue(t.Context(), bus.ConsulKVBucket)
		return err == nil, err
	})
	return js, kv
}

// readJobEvents fetches from the consumer until it has a Job event of the
// job, acknowledging what it reads.
func readJobEvent(t *testing.T, cons jetstream.Consumer, jobID string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		batch, err := cons.Fetch(10, jetstream.FetchMaxWait(time.Second))
		must.NoError(t, err)
		found := false
		for msg := range batch.Messages() {
			must.NoError(t, msg.DoubleAck(t.Context()))
			if msg.Headers().Get(bus.HeaderTopic) == "Job" && strings.Contains(string(msg.Data()), jobID) {
				found = true
			}
		}
		if found {
			return
		}
	}
	t.Fatalf("the consumer did not deliver the Job event of %s", jobID)
}

// A reader's JetStream request names its reply subject, and NATS does not
// check a reply subject against the publish rights (review finding SRV-1):
// inside the bus account the answer was written, with the rights of
// JetStream, to whatever the reader named, the key of another namespace in
// the Consul KV bucket or a subject of an event stream. A reader is now in
// an account of its own, where its requests reach its consumers through
// service imports and a reply subject is a subject of that account only.
func TestCDC_ReaderReplySubject(t *testing.T) {
	ci.Parallel(t)

	s1, _, cleanup := TestACLServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	js, kv := cdcTestBus(t, s1)

	reader := cdcTestTokens(t, s1, map[string]string{
		"cdc-reader": `cdc { receive = ["norca.global.default.>"] }`,
	})["cdc-reader"]
	cdcTestConsumer(t, s1, reader, "norca.global.default.Job")
	consumer := bus.EventStreamName("global") + "." + reader.AccessorID

	// Something for the consumer to deliver.
	job := mock.Job()
	must.NoError(t, s1.fsm.State().UpsertJob(structs.JobRegisterRequestType, 200, nil, job))
	lastJobEvent(t, js, job.ID)
	events, err := js.Stream(t.Context(), bus.EventStreamName("global"))
	must.NoError(t, err)
	countOther := func() uint64 {
		info, err := events.Info(t.Context(), jetstream.WithSubjectFilter("norca.global.other.Job"))
		must.NoError(t, err)
		return info.State.Subjects["norca.global.other.Job"]
	}
	must.Eq(t, uint64(0), countOther())

	nc, err := nats.Connect(busURL(s1), nats.Token(reader.SecretID))
	must.NoError(t, err)
	defer nc.Close()

	// The answers arrive where the reader asked for them: in its own
	// account. (Before the fix these subscriptions were refused, and the
	// answers went into the bucket and the stream.)
	key := "other-ns.consul=2Fkv.db.password"
	kvReplies, _ := nc.SubscribeSync("$KV." + bus.ConsulKVBucket + "." + key)
	eventReplies, _ := nc.SubscribeSync("norca.global.other.Job")
	must.NoError(t, nc.Flush())

	must.NoError(t, nc.PublishRequest("$JS.API.INFO", "$KV."+bus.ConsulKVBucket+"."+key, nil))
	must.NoError(t, nc.PublishRequest("$JS.API.CONSUMER.INFO."+consumer, "$KV."+bus.ConsulKVBucket+"."+key, nil))
	must.NoError(t, nc.PublishRequest("$JS.API.CONSUMER.MSG.NEXT."+consumer, "norca.global.other.Job",
		[]byte(`{"batch":1,"expires":2000000000}`)))
	must.NoError(t, nc.Flush())
	time.Sleep(2 * time.Second)

	entry, err := kv.Get(t.Context(), key)
	if err == nil {
		t.Fatalf("the reader wrote the Consul KV key of another namespace with the reply of its request: revision %d, value %.60s",
			entry.Revision(), entry.Value())
	}
	must.True(t, errors.Is(err, jetstream.ErrKeyNotFound), must.Sprint(err))
	must.Eq(t, uint64(0), countOther(), must.Sprint("the reader wrote an event of another namespace with the delivery of its fetch"))

	// The requests were served, not dropped.
	msg, err := kvReplies.NextMsg(5 * time.Second)
	must.NoError(t, err)
	must.StrContains(t, string(msg.Data), "io.nats.jetstream.api.v1")
	msg, err = eventReplies.NextMsg(5 * time.Second)
	must.NoError(t, err)
	must.StrContains(t, string(msg.Data), job.ID)
}

// A token with a cdc rule only reads as before, without a mark and with the
// inbox it likes: the stream information, its consumer, fetches and
// acknowledgements, and the changes live. Other readers' consumers and the
// rest of the bus are out of reach.
func TestCDC_ReaderAccount_CDCOnly(t *testing.T) {
	ci.Parallel(t)

	s1, _, cleanup := TestACLServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	_, kv := cdcTestBus(t, s1)

	tokens := cdcTestTokens(t, s1, map[string]string{
		"reader-a": `cdc { receive = ["norca.global.default.>"] }`,
		"reader-b": `cdc { receive = ["norca.>"] }`,
	})
	a, b := tokens["reader-a"], tokens["reader-b"]
	cdcTestConsumer(t, s1, a, "norca.global.default.Job")
	cdcTestConsumer(t, s1, b, "norca.global.default.Job")
	_, err := kv.Put(t.Context(), "default.consul=2Fkv.app.secret", []byte("sealed"))
	must.NoError(t, err)

	for name, token := range map[string]*structs.ACLToken{"a": a, "b": b} {
		// Two connections of the reader: its account takes more than one.
		for _, opts := range [][]nats.Option{
			{nats.Token(token.SecretID)},
			{nats.Token(token.SecretID), nats.CustomInboxPrefix(bus.InboxPrefix + "." + token.AccessorID)},
			{nats.UserInfo(bus.ReaderUser, token.SecretID)},
		} {
			nc, err := nats.Connect(busURL(s1), opts...)
			must.NoError(t, err, must.Sprint(name))
			defer nc.Close()
			live := &subjects{}
			_, err = nc.Subscribe("norca.>", live.add)
			must.NoError(t, err)
			must.NoError(t, nc.Flush())

			readerJS, err := jetstream.New(nc)
			must.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			_, err = readerJS.AccountInfo(ctx)
			must.NoError(t, err, must.Sprint(name))
			_, err = readerJS.Stream(ctx, bus.EventStreamName("global"))
			must.NoError(t, err, must.Sprint(name))
			cons, err := readerJS.Consumer(ctx, bus.EventStreamName("global"), token.AccessorID)
			must.NoError(t, err, must.Sprint(name))

			job := mock.Job()
			index, _ := s1.fsm.State().LatestIndex()
			must.NoError(t, s1.fsm.State().UpsertJob(structs.JobRegisterRequestType, index+1, nil, job))
			readJobEvent(t, cons, job.ID)
			waitBus(t, func() (bool, error) { return live.has("Job"), nil })

			// Out of reach: the consumer of the other reader, making a
			// consumer, the Consul KV bucket, publishing an event.
			other := b
			if token == b {
				other = a
			}
			short, cancelShort := context.WithTimeout(t.Context(), time.Second)
			_, err = readerJS.Consumer(short, bus.EventStreamName("global"), other.AccessorID)
			must.Error(t, err)
			_, err = readerJS.CreateConsumer(short, bus.EventStreamName("global"), jetstream.ConsumerConfig{Durable: token.AccessorID + "x"})
			must.Error(t, err)
			_, err = readerJS.KeyValue(short, bus.ConsulKVBucket)
			must.Error(t, err)
			cancelShort()
			_ = nc.Publish("norca.global.default.Job", []byte("forged"))
			_ = nc.Publish("$KV."+bus.ConsulKVBucket+".default.consul=2Fkv.app.secret", []byte("forged"))
			must.NoError(t, nc.Flush())
		}
	}
	time.Sleep(500 * time.Millisecond)
	entry, err := kv.Get(t.Context(), "default.consul=2Fkv.app.secret")
	must.NoError(t, err)
	must.Eq(t, "sealed", string(entry.Value()))
	must.Eq(t, uint64(1), entry.Revision())
}

// A token with a bus rule and a cdc rule uses two connections: the plain
// one is in the bus account with its bus rule (and the changes live), and
// has no JetStream; the one that logs in as the user "cdc" with the token
// as password is the reader.
func TestCDC_ReaderAccount_MixedToken(t *testing.T) {
	ci.Parallel(t)

	s1, _, cleanup := TestACLServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	_, kv := cdcTestBus(t, s1)

	tokens := cdcTestTokens(t, s1, map[string]string{
		"mixed": `
bus { publish = ["app.>"] subscribe = ["app.>"] }
cdc { receive = ["norca.global.default.>"] }`,
		"bus-only": `bus { publish = ["app.>"] subscribe = ["app.>"] }`,
	})
	mixed, busOnly := tokens["mixed"], tokens["bus-only"]
	cdcTestConsumer(t, s1, mixed, "norca.global.default.Job")
	consumer := bus.EventStreamName("global") + "." + mixed.AccessorID

	// The bus connection: the bus rule works, with another token of the bus.
	busErrs := make(chan error, 8)
	busConn, err := nats.Connect(busURL(s1), nats.Token(mixed.SecretID),
		nats.CustomInboxPrefix(bus.InboxPrefix+"."+mixed.AccessorID),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) { busErrs <- err }))
	must.NoError(t, err)
	defer busConn.Close()
	peer, err := nats.Connect(busURL(s1), nats.Token(busOnly.SecretID))
	must.NoError(t, err)
	defer peer.Close()
	app, err := peer.SubscribeSync("app.hello")
	must.NoError(t, err)
	must.NoError(t, peer.Flush())
	live := &subjects{}
	_, err = busConn.Subscribe("norca.global.default.>", live.add)
	must.NoError(t, err)
	must.NoError(t, busConn.Publish("app.hello", []byte("hi")))
	msg, err := app.NextMsg(5 * time.Second)
	must.NoError(t, err)
	must.Eq(t, "hi", string(msg.Data))

	// It has no JetStream: the request is a permissions violation and
	// nothing answers, whatever the reply subject.
	key := "other-ns.consul=2Fkv.db.password"
	must.NoError(t, busConn.PublishRequest("$JS.API.INFO", "$KV."+bus.ConsulKVBucket+"."+key, nil))
	must.NoError(t, busConn.PublishRequest("$JS.API.CONSUMER.MSG.NEXT."+consumer, "$KV."+bus.ConsulKVBucket+"."+key,
		[]byte(`{"batch":1,"expires":1000000000}`)))
	must.NoError(t, busConn.Flush())
	select {
	case err := <-busErrs:
		must.ErrorContains(t, err, "Permissions Violation")
	case <-time.After(5 * time.Second):
		t.Fatal("the bus connection of a token with a cdc rule still reaches JetStream")
	}

	// The reader connection of the same token.
	readerConn, err := nats.Connect(busURL(s1), nats.UserInfo(bus.ReaderUser, mixed.SecretID))
	must.NoError(t, err)
	defer readerConn.Close()
	readerJS, err := jetstream.New(readerConn)
	must.NoError(t, err)
	cons, err := readerJS.Consumer(t.Context(), bus.EventStreamName("global"), mixed.AccessorID)
	must.NoError(t, err)
	job := mock.Job()
	index, _ := s1.fsm.State().LatestIndex()
	must.NoError(t, s1.fsm.State().UpsertJob(structs.JobRegisterRequestType, index+1, nil, job))
	readJobEvent(t, cons, job.ID)
	// The changes also reach the bus connection live.
	waitBus(t, func() (bool, error) { return live.has("Job"), nil })

	// The reader connection is not on the bus: it can't publish to the
	// subjects of the bus rule.
	_ = readerConn.Publish("app.hello", []byte("from the reader"))
	must.NoError(t, readerConn.Flush())
	_, err = app.NextMsg(time.Second)
	must.ErrorIs(t, err, nats.ErrTimeout)

	_, err = kv.Get(t.Context(), key)
	must.True(t, errors.Is(err, jetstream.ErrKeyNotFound), must.Sprint(err))

	// A token without a cdc rule is no reader.
	_, err = nats.Connect(busURL(s1), nats.UserInfo(bus.ReaderUser, busOnly.SecretID))
	must.Error(t, err)
}

// A reader's account is made on the server it connects to; its consumer
// may live on another server of the bus cluster. Every server serves the
// reader, and none lets the reply of its request into the bus account.
func TestCDC_ReaderAccount_Cluster(t *testing.T) {
	ci.Parallel(t)

	cb := func(c *Config) {
		enableBus(t)(c)
		c.BootstrapExpect = 3
		c.ACLEnabled = true
	}
	var servers []*Server
	for i := 0; i < 3; i++ {
		s, cleanup := TestServer(t, cb)
		defer cleanup()
		servers = append(servers, s)
	}
	TestJoin(t, servers...)
	for _, s := range servers {
		testutil.WaitForLeader(t, s.RPC)
	}
	s1 := servers[0]
	waitBus(t, func() (bool, error) {
		for _, s := range servers {
			st, err := s.busStatus()
			if err != nil {
				return false, err
			}
			if len(st.Routes) < 2 || st.JetStream == nil || st.JetStream.MetaLeader == "" || st.JetStream.ClusterSize != 3 {
				return false, errors.New(st.Server + ": the bus cluster is not formed")
			}
		}
		return true, nil
	})

	// Through Raft: every server authenticates the reader.
	var bootstrap structs.ACLTokenUpsertResponse
	must.NoError(t, s1.RPC("ACL.Bootstrap", &structs.ACLTokenBootstrapRequest{
		WriteRequest: structs.WriteRequest{Region: "global"},
	}, &bootstrap))
	root := bootstrap.Tokens[0]
	write := structs.WriteRequest{Region: "global", AuthToken: root.SecretID}
	must.NoError(t, s1.RPC("ACL.UpsertPolicies", &structs.ACLPolicyUpsertRequest{
		Policies:     []*structs.ACLPolicy{{Name: "cdc-reader", Rules: `cdc { receive = ["norca.global.default.>"] }`}},
		WriteRequest: write,
	}, new(structs.GenericResponse)))
	var tokens structs.ACLTokenUpsertResponse
	must.NoError(t, s1.RPC(structs.ACLUpsertTokensRPCMethod, &structs.ACLTokenUpsertRequest{
		Tokens:       []*structs.ACLToken{{Name: "reader", Type: structs.ACLClientToken, Policies: []string{"cdc-reader"}}},
		WriteRequest: write,
	}, &tokens))
	reader := tokens.Tokens[0]
	waitBus(t, func() (bool, error) {
		for _, s := range servers {
			if tok, err := s.fsm.State().ACLTokenBySecretID(nil, reader.SecretID); err != nil || tok == nil {
				return false, err
			}
		}
		return true, nil
	})

	js, kv := cdcTestBus(t, s1)
	waitBus(t, func() (bool, error) {
		var resp structs.CDCConsumerWriteResponse
		err := s1.RPC(structs.CDCConsumerUpsertRPCMethod, &structs.CDCConsumerUpsertRequest{
			Stream:       structs.CDCStreamEvents,
			Consumer:     structs.CDCConsumerSpec{Filters: []string{"norca.global.default.Job"}},
			WriteRequest: structs.WriteRequest{Region: "global", AuthToken: reader.SecretID},
		}, &resp)
		return err == nil, err
	})
	consumer := bus.EventStreamName("global") + "." + reader.AccessorID
	key := "other-ns.consul=2Fkv.db.password"

	for i, s := range servers {
		nc, err := nats.Connect(busURL(s), nats.Token(reader.SecretID))
		must.NoError(t, err, must.Sprintf("server %d", i))
		defer nc.Close()
		readerJS, err := jetstream.New(nc)
		must.NoError(t, err)
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		_, err = readerJS.Stream(ctx, bus.EventStreamName("global"))
		must.NoError(t, err, must.Sprintf("server %d", i))
		cons, err := readerJS.Consumer(ctx, bus.EventStreamName("global"), reader.AccessorID)
		must.NoError(t, err, must.Sprintf("server %d", i))

		job := mock.Job()
		must.NoError(t, s1.RPC("Job.Register", &structs.JobRegisterRequest{
			Job:          job,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace, AuthToken: root.SecretID},
		}, new(structs.JobRegisterResponse)))
		lastJobEvent(t, js, job.ID)
		readJobEvent(t, cons, job.ID)

		must.NoError(t, nc.PublishRequest("$JS.API.CONSUMER.INFO."+consumer, "$KV."+bus.ConsulKVBucket+"."+key, nil))
		must.NoError(t, nc.PublishRequest("$JS.API.INFO", "$KV."+bus.ConsulKVBucket+"."+key, nil))
		must.NoError(t, nc.Flush())
	}
	time.Sleep(2 * time.Second)
	_, err := kv.Get(t.Context(), key)
	must.True(t, errors.Is(err, jetstream.ErrKeyNotFound), must.Sprint(err))
}

// The stream information a reader may ask for says nothing about the
// subjects of the stream (review finding SEC-16): with a subjects filter
// the JetStream API lists every subject with its message count, the
// namespaces outside the reader's cdc rule and their activity among them.
// The servers answer the reader's request themselves.
func TestCDC_ReaderStreamInfo(t *testing.T) {
	ci.Parallel(t)

	s1, _, cleanup := TestACLServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	js, _ := cdcTestBus(t, s1)
	events := bus.EventStreamName("global")

	reader := cdcTestTokens(t, s1, map[string]string{
		"cdc-reader": `cdc { receive = ["norca.global.default.>"] }`,
	})["cdc-reader"]
	cdcTestConsumer(t, s1, reader, "norca.global.default.Job")

	// A change of a namespace outside the rule.
	st := s1.fsm.State()
	index, _ := st.LatestIndex()
	must.NoError(t, st.UpsertNamespaces(index+1, []*structs.Namespace{{Name: "payroll"}}))
	job := mock.Job()
	job.Namespace = "payroll"
	must.NoError(t, st.UpsertJob(structs.JobRegisterRequestType, index+2, nil, job))
	waitBus(t, func() (bool, error) {
		stream, err := js.Stream(t.Context(), events)
		if err != nil {
			return false, err
		}
		_, err = stream.GetLastMsgForSubject(t.Context(), "norca.global.payroll.Job")
		return err == nil, err
	})

	nc, err := nats.Connect(busURL(s1), nats.Token(reader.SecretID))
	must.NoError(t, err)
	defer nc.Close()
	for _, request := range []string{`{"subjects_filter":">"}`, `{"subjects_filter":"norca.global.payroll.>"}`, `{"deleted_details":true}`, ``} {
		msg, err := nc.Request("$JS.API.STREAM.INFO."+events, []byte(request), 10*time.Second)
		must.NoError(t, err, must.Sprint(request))
		must.StrNotContains(t, string(msg.Data), "payroll", must.Sprintf("request %s", request))
		must.StrNotContains(t, string(msg.Data), `"subjects":{`, must.Sprintf("request %s", request))
		must.StrContains(t, string(msg.Data), `"name":"`+events+`"`)
	}

	// The clients still get the stream: its configuration and its state.
	readerJS, err := jetstream.New(nc)
	must.NoError(t, err)
	stream, err := readerJS.Stream(t.Context(), events)
	must.NoError(t, err)
	info, err := stream.Info(t.Context(), jetstream.WithSubjectFilter(">"))
	must.NoError(t, err)
	must.Eq(t, events, info.Config.Name)
	must.Positive(t, info.State.Msgs)
	must.MapEmpty(t, info.State.Subjects)

	// A stream the reader has no consumer of, and a request that did not
	// come from a reader's account, get no answer.
	_, err = nc.Request("$JS.API.STREAM.INFO."+bus.AdminEventStreamName("global"), nil, time.Second)
	must.Error(t, err)
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
)

// busTestConn connects a token to the bus with its own inbox prefix and
// returns the connection with the errors the server sends on its own.
func busTestConn(t *testing.T, s *Server, token *structs.ACLToken) (*nats.Conn, chan error) {
	t.Helper()
	errs := make(chan error, 64)
	nc, err := nats.Connect(busURL(s), nats.Token(token.SecretID),
		nats.CustomInboxPrefix(bus.InboxPrefix+"."+token.AccessorID),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			select {
			case errs <- err:
			default:
			}
		}))
	must.NoError(t, err)
	t.Cleanup(nc.Close)
	return nc, errs
}

// busKVAbsent reports a key of the Consul KV bucket that exists. The test
// goes on, to show everything a route reaches.
func busKVAbsent(t *testing.T, kv jetstream.KeyValue, key, how string) {
	t.Helper()
	entry, err := kv.Get(t.Context(), key)
	switch {
	case err == nil:
		t.Errorf("%s wrote the Consul KV key %s of another namespace: revision %d, value %.80s",
			how, key, entry.Revision(), entry.Value())
	case !errors.Is(err, jetstream.ErrKeyNotFound):
		t.Errorf("reading the key %s: %v", key, err)
	}
}

// A connection with a bus rule that lets it publish and subscribe to one
// subject sent itself a message with a reply subject of its choice; NATS
// then let it publish once to that subject, whatever its rule said (review
// finding SEC-1): the JetStream API of the bus account, the Consul KV bucket,
// the usage reports of any node.
func TestBus_ResponderRoute(t *testing.T) {
	ci.Parallel(t)

	s1, _, cleanup := TestACLServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	js, kv := cdcTestBus(t, s1)
	events := bus.EventStreamName("global")
	telemetry := bus.TelemetryStreamName("global")
	waitBus(t, func() (bool, error) {
		_, err := js.Stream(t.Context(), telemetry)
		return err == nil, err
	})

	token := cdcTestTokens(t, s1, map[string]string{
		"bus-app": `bus { publish = ["app.>"] subscribe = ["app.>"] }`,
	})["bus-app"]
	nc, _ := busTestConn(t, s1, token)
	sub, err := nc.SubscribeSync("app.x")
	must.NoError(t, err)
	must.NoError(t, nc.Flush())

	// The connection's own message comes back with the reply subject; then
	// it publishes to that subject.
	viaReply := func(target string, payload []byte) {
		t.Helper()
		must.NoError(t, nc.PublishRequest("app.x", target, []byte("x")))
		msg, err := sub.NextMsg(5 * time.Second)
		must.NoError(t, err)
		must.Eq(t, target, msg.Reply)
		must.NoError(t, nc.Publish(target, payload))
		must.NoError(t, nc.Flush())
	}

	key := "other-ns.consul=2Fkv.db.password"
	viaReply("$KV."+bus.ConsulKVBucket+"."+key, []byte("owned"))
	viaReply("$JS.API.CONSUMER.CREATE."+events+".evil",
		[]byte(`{"stream_name":"`+events+`","config":{"durable_name":"evil","name":"evil","ack_policy":"explicit"}}`))
	node := structs.BusTelemetrySubject("global", "11111111-2222-3333-4444-555555555555")
	viaReply(node, []byte("{}"))
	time.Sleep(2 * time.Second)

	busKVAbsent(t, kv, key, "a connection with a bus rule")
	if _, err = js.Consumer(t.Context(), events, "evil"); !errors.Is(err, jetstream.ErrConsumerNotFound) {
		t.Errorf("a connection with a bus rule made a consumer of the event stream (error of the lookup: %v)", err)
	}
	stream, err := js.Stream(t.Context(), telemetry)
	must.NoError(t, err)
	if _, err = stream.GetLastMsgForSubject(t.Context(), node); !errors.Is(err, jetstream.ErrMsgNotFound) {
		t.Errorf("a connection with a bus rule published the usage report of a node (error of the lookup: %v)", err)
	}
}

// A stream acknowledges a stored message to the reply subject its publisher
// named, with the rights of JetStream (review finding SEC-2): a node, with
// the reply of a usage report, and a token that may publish the pack push
// notice purged a stream (the acknowledgement is a valid purge request) and
// wrote a key of the Consul KV bucket.
func TestBus_PubAckRoute(t *testing.T) {
	ci.Parallel(t)

	s1, _, cleanup := TestACLServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	js, kv := cdcTestBus(t, s1)
	eventsName := bus.EventStreamName("global")
	waitBus(t, func() (bool, error) {
		for _, name := range []string{bus.TelemetryStreamName("global"), pushStreamName} {
			if _, err := js.Stream(t.Context(), name); err != nil {
				return false, err
			}
		}
		return true, nil
	})

	// Events to lose.
	st := s1.fsm.State()
	var job *structs.Job
	for i := range 3 {
		job = mock.Job()
		must.NoError(t, st.UpsertJob(structs.JobRegisterRequestType, uint64(200+i), nil, job))
	}
	lastJobEvent(t, js, job.ID)
	events, err := js.Stream(t.Context(), eventsName)
	must.NoError(t, err)
	before, err := events.Info(t.Context())
	must.NoError(t, err)
	must.Positive(t, before.State.Msgs)

	// A node: its usage reports, the last ones with a reply subject.
	node := mock.Node()
	must.NoError(t, st.UpsertNode(structs.MsgTypeTestSetup, 210, node))
	nodeConn, err := nats.Connect(busURL(s1), nats.UserInfo(node.ID, node.SecretID))
	must.NoError(t, err)
	defer nodeConn.Close()
	subject := structs.BusTelemetrySubject("global", node.ID)
	for range 100 {
		must.NoError(t, nodeConn.Publish(subject, []byte("{}")))
	}
	nodeKey := "other-ns.consul=2Fkv.from.node"
	must.NoError(t, nodeConn.PublishRequest(subject, "$JS.API.STREAM.PURGE."+eventsName, []byte("{}")))
	must.NoError(t, nodeConn.PublishRequest(subject, "$KV."+bus.ConsulKVBucket+"."+nodeKey, []byte("{}")))
	must.NoError(t, nodeConn.Flush())

	// A token that may publish the pack push notice.
	pusher := cdcTestTokens(t, s1, map[string]string{
		"pusher": `bus { publish = ["norca.packs.pushed", "norca.packs.pushed.>"] }`,
	})["pusher"]
	pushConn, _ := busTestConn(t, s1, pusher)
	pushKey := "other-ns.consul=2Fkv.from.push"
	for _, subject := range []string{"norca.packs.pushed", "norca.packs.pushed.ci"} {
		must.NoError(t, pushConn.PublishRequest(subject, "$KV."+bus.ConsulKVBucket+"."+pushKey, []byte(`{"Registry":"none"}`)))
	}
	must.NoError(t, pushConn.Flush())
	time.Sleep(2 * time.Second)

	busKVAbsent(t, kv, nodeKey, "the acknowledgement of a node's usage report")
	busKVAbsent(t, kv, pushKey, "the acknowledgement of a pack push notice")
	after, err := events.Info(t.Context())
	must.NoError(t, err)
	if after.State.FirstSeq != before.State.FirstSeq || after.State.Msgs < before.State.Msgs {
		t.Errorf("the acknowledgement of a node's usage report purged the event stream: %d messages from sequence %d before, %d from %d after",
			before.State.Msgs, before.State.FirstSeq, after.State.Msgs, after.State.FirstSeq)
	}
}

// concreteSubject replaces the wildcards of a subject with tokens.
func concreteSubject(subject string) string {
	tokens := strings.Split(subject, ".")
	for i, tok := range tokens {
		switch tok {
		case "*":
			tokens[i] = "x"
		case ">":
			tokens[i] = "y"
		}
	}
	return strings.Join(tokens, ".")
}

// Nothing in the bus account sends a message to a subject that a connection
// without every right chose as the reply of what it published: for every
// stream of the account, either it acknowledges nothing, or no bus rule
// reaches its subjects; no service of the NATS system account answers such
// a connection; and a connection that receives a request publishes its
// answer by its own rule only.
func TestBus_NoMessageToChosenReply(t *testing.T) {
	ci.Parallel(t)

	s1, root, cleanup := TestACLServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	js, _ := cdcTestBus(t, s1)
	waitBus(t, func() (bool, error) {
		for _, name := range []string{bus.TelemetryStreamName("global"), pushStreamName, bus.AdminEventStreamName("global"),
			bus.ConsulKVChangesStreamName("global")} {
			if _, err := js.Stream(t.Context(), name); err != nil {
				return false, err
			}
		}
		return true, nil
	})

	// Every reply subject of the test is below canary.>, which a connection
	// with every right watches.
	watcher, err := nats.Connect(busURL(s1), nats.Token(root.SecretID))
	must.NoError(t, err)
	defer watcher.Close()
	canary, err := watcher.SubscribeSync("canary.>")
	must.NoError(t, err)
	must.NoError(t, watcher.Flush())

	// The subjects of every stream that acknowledges, and the services every
	// account imports from the NATS system account, each named by the bus
	// rule of a token of its own (the state takes rules the ACL endpoint
	// would refuse).
	targets := map[string]string{
		"sys-user-info": "$SYS.REQ.USER.INFO",
		"sys-statz":     "$SYS.REQ.ACCOUNT.PING.STATZ",
		"sys-connz":     "$SYS.REQ.ACCOUNT.PING.CONNZ",
		"js-info":       "$JS.API.INFO",
	}
	streams := 0
	lister := js.ListStreams(t.Context())
	for info := range lister.Info() {
		streams++
		if info.Config.NoAck {
			continue
		}
		for i, subject := range info.Config.Subjects {
			targets[fmt.Sprintf("stream-%s-%d", info.Config.Name, i)] = subject
		}
	}
	must.NoError(t, lister.Err())
	must.True(t, streams >= 5, must.Sprintf("only %d streams listed", streams))

	rules := map[string]string{}
	for name, subject := range targets {
		rules[name] = fmt.Sprintf(`bus { publish = [%q] subscribe = ["app.>"] }`, subject)
	}
	tokens := cdcTestTokens(t, s1, rules)
	for name, subject := range targets {
		nc, err := nats.Connect(busURL(s1), nats.Token(tokens[name].SecretID))
		if err != nil {
			continue // a rule that grants nothing: no connection
		}
		must.NoError(t, nc.PublishRequest(concreteSubject(subject), "canary."+name, []byte("{}")))
		must.NoError(t, nc.Flush())
		nc.Close()
	}

	// The usage report of a node is stored and not acknowledged.
	node := mock.Node()
	must.NoError(t, s1.fsm.State().UpsertNode(structs.MsgTypeTestSetup, 300, node))
	nodeConn, err := nats.Connect(busURL(s1), nats.UserInfo(node.ID, node.SecretID))
	must.NoError(t, err)
	defer nodeConn.Close()
	report := structs.BusTelemetrySubject("global", node.ID)
	must.NoError(t, nodeConn.PublishRequest(report, "canary.node", []byte("{}")))
	must.NoError(t, nodeConn.Flush())
	waitBus(t, func() (bool, error) {
		stream, err := js.Stream(t.Context(), bus.TelemetryStreamName("global"))
		if err != nil {
			return false, err
		}
		_, err = stream.GetLastMsgForSubject(t.Context(), report)
		return err == nil, err
	})

	// A responder answers by its own rule: not to a subject the requester
	// chose outside it, and not with the right NATS gives a tracked reply.
	apps := cdcTestTokens(t, s1, map[string]string{
		"requester": `bus { publish = ["app.>"] subscribe = ["app.>"] }`,
		"responder": `bus { publish = ["app.>"] subscribe = ["app.>"] }`,
	})
	requester, _ := busTestConn(t, s1, apps["requester"])
	responder, responderErrs := busTestConn(t, s1, apps["responder"])
	_, err = responder.Subscribe("app.svc", func(m *nats.Msg) { _ = m.Respond([]byte("answer")) })
	must.NoError(t, err)
	must.NoError(t, responder.Flush())
	must.NoError(t, requester.PublishRequest("app.svc", "canary.responder", []byte("q")))
	must.NoError(t, requester.Flush())
	select {
	case err := <-responderErrs:
		must.ErrorContains(t, err, "Permissions Violation")
	case <-time.After(5 * time.Second):
		t.Fatal("the responder was not refused the reply subject outside its rule")
	}

	if msg, err := canary.NextMsg(3 * time.Second); err == nil {
		t.Fatalf("a message reached the chosen reply subject %s: %.120s", msg.Subject, msg.Data)
	} else {
		must.ErrorIs(t, err, nats.ErrTimeout)
	}
}

// Request and reply between connections with bus rules: the requester gets
// its replies under its own inbox prefix, and the responder needs a rule
// that lets it publish there. Without it the responder is refused, and no
// rule lets a connection subscribe to the inbox of another.
func TestBus_RequestReply(t *testing.T) {
	ci.Parallel(t)

	s1, _, cleanup := TestACLServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)

	tokens := cdcTestTokens(t, s1, map[string]string{
		"requester": `bus { publish = ["app.>"] subscribe = ["app.>"] }`,
		"replies-to-all": `bus {
  subscribe = ["app.all"]
  publish   = ["_INBOX.>"]
}`,
		"replies-to-none": `bus { subscribe = ["app.none"] }`,
	})
	requester, _ := busTestConn(t, s1, tokens["requester"])
	// A responder whose rule names the inbox of this requester only.
	one := cdcTestTokens(t, s1, map[string]string{
		"replies-to-one": fmt.Sprintf(`bus {
  subscribe = ["app.one"]
  publish   = ["_INBOX.%s.>"]
}`, tokens["requester"].AccessorID),
		"other-requester": `bus { publish = ["app.>"] }`,
	})

	serve := func(token *structs.ACLToken, subject string) chan error {
		nc, errs := busTestConn(t, s1, token)
		_, err := nc.Subscribe(subject, func(m *nats.Msg) { _ = m.Respond([]byte("answer from " + subject)) })
		must.NoError(t, err)
		must.NoError(t, nc.Flush())
		return errs
	}
	serve(tokens["replies-to-all"], "app.all")
	noneErrs := serve(tokens["replies-to-none"], "app.none")
	oneErrs := serve(one["replies-to-one"], "app.one")

	for _, subject := range []string{"app.all", "app.one"} {
		msg, err := requester.Request(subject, []byte("q"), 5*time.Second)
		must.NoError(t, err, must.Sprint(subject))
		must.Eq(t, "answer from "+subject, string(msg.Data))
	}

	// No right to publish to the inboxes: the request gets no answer.
	_, err := requester.Request("app.none", []byte("q"), 2*time.Second)
	must.Error(t, err)
	select {
	case err := <-noneErrs:
		must.ErrorContains(t, err, "Permissions Violation")
	case <-time.After(5 * time.Second):
		t.Fatal("a responder without the right answered into an inbox")
	}

	// The inbox of another requester is outside the rule of replies-to-one.
	other, _ := busTestConn(t, s1, one["other-requester"])
	_, err = other.Request("app.one", []byte("q"), 2*time.Second)
	must.Error(t, err)
	select {
	case err := <-oneErrs:
		must.ErrorContains(t, err, "Permissions Violation")
	case <-time.After(5 * time.Second):
		t.Fatal("a responder answered into an inbox outside its rule")
	}

	// The right to publish into the inboxes is not a right to read them (a
	// rule that asks for that is no valid policy, see the acl package).
	reader, readerErrs := busTestConn(t, s1, tokens["replies-to-all"])
	_, err = reader.SubscribeSync("_INBOX.>")
	must.NoError(t, err)
	must.NoError(t, reader.Flush())
	select {
	case err := <-readerErrs:
		must.ErrorContains(t, err, "Permissions Violation")
	case <-time.After(5 * time.Second):
		t.Fatal("a bus rule let a connection subscribe to every inbox")
	}
}

// A pack push notice a pipeline publishes on the bus is stored by a server,
// without an acknowledgement to the publisher, and synced like one sent
// through the API.
func TestBusEvents_PushNoticeRelay(t *testing.T) {
	ci.Parallel(t)

	s1, _, cleanup := TestACLServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	js, _ := cdcTestBus(t, s1)
	waitBus(t, func() (bool, error) {
		_, err := js.Stream(t.Context(), pushStreamName)
		return err == nil, err
	})

	pusher := cdcTestTokens(t, s1, map[string]string{
		"pusher": `bus { publish = ["norca.packs.pushed", "norca.packs.pushed.>"] }`,
	})["pusher"]
	nc, errs := busTestConn(t, s1, pusher)

	stored := func() uint64 {
		stream, err := js.Stream(t.Context(), pushStreamName)
		must.NoError(t, err)
		info, err := stream.Info(t.Context())
		must.NoError(t, err)
		return info.State.LastSeq
	}
	before := stored()
	// The relay of this server subscribes when its loop starts.
	waitBus(t, func() (bool, error) {
		if err := nc.Publish("norca.packs.pushed", []byte(`{"Registry":"relay-test"}`)); err != nil {
			return false, err
		}
		if err := nc.Flush(); err != nil {
			return false, err
		}
		return stored() > before, errors.New("the notice was not stored")
	})
	stream, err := js.Stream(t.Context(), pushStreamName)
	must.NoError(t, err)
	msg, err := stream.GetMsg(t.Context(), stored())
	must.NoError(t, err)
	must.StrContains(t, string(msg.Data), "relay-test")

	// The publisher gets no acknowledgement, and can't write the stored
	// subject itself.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err = nc.RequestWithContext(ctx, "norca.packs.pushed", []byte(`{"Registry":"relay-test"}`))
	must.Error(t, err)
	must.NoError(t, nc.Publish("norca."+bus.PushStoredSubject, []byte(`{"Registry":"forged"}`)))
	must.NoError(t, nc.Flush())
	select {
	case err := <-errs:
		must.ErrorContains(t, err, "Permissions Violation")
	case <-time.After(5 * time.Second):
		t.Fatal("a token published the stored subject of the push notices")
	}
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"
)

// spy collects what a connection receives on the subjects it tries.
type spy struct {
	mu   sync.Mutex
	seen []string
	errs []string
}

func (s *spy) message(m *nats.Msg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, m.Subject+" "+string(m.Data))
}

func (s *spy) failure(_ *nats.Conn, _ *nats.Subscription, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs = append(s.errs, err.Error())
}

func (s *spy) result() (seen, errs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...), append([]string(nil), s.errs...)
}

// TestBusServer_RepliesArePrivate checks that a connection reads the replies
// of its own requests only. All connections share one NATS account: with a
// common "_INBOX.>" a node, which may only publish its telemetry, received
// the values of the Consul KV the servers read and every JetStream reply
// (found by a probe on 2026-11-14: 6 messages, 2 of them with the value).
func TestBusServer_RepliesArePrivate(t *testing.T) {
	ci.Parallel(t)

	s1, _, cleanup := TestACLServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	st := s1.fsm.State()

	node := mock.Node()
	must.NoError(t, st.UpsertNode(structs.MsgTypeTestSetup, 100, node))
	policy := mock.ACLPolicy()
	policy.Rules = `bus {
  subscribe = ["norca.>", "svc.>"]
  publish   = ["svc.>"]
}`
	policy.SetHash()
	must.NoError(t, st.UpsertACLPolicies(structs.MsgTypeTestSetup, 101, []*structs.ACLPolicy{policy}))
	tokenA, tokenB := mock.ACLToken(), mock.ACLToken()
	tokenA.Policies, tokenB.Policies = []string{policy.Name}, []string{policy.Name}
	tokenA.SetHash()
	tokenB.SetHash()
	must.NoError(t, st.UpsertACLTokens(structs.MsgTypeTestSetup, 102, []*structs.ACLToken{tokenA, tokenB}))
	inboxA := bus.InboxPrefix + "." + tokenA.AccessorID

	// The server reads and writes the Consul KV, and answers a service.
	srv, err := s1.bus.Connect()
	must.NoError(t, err)
	defer srv.Close()
	js, err := jetstream.New(srv)
	must.NoError(t, err)
	var kv jetstream.KeyValue
	waitBus(t, func() (bool, error) {
		kv, err = js.KeyValue(t.Context(), bus.ConsulKVBucket)
		return err == nil, err
	})
	_, err = srv.Subscribe("svc.echo", func(m *nats.Msg) { _ = m.Respond([]byte("REPLY-FOR-A")) })
	must.NoError(t, err)
	must.NoError(t, srv.Flush())

	// The listeners: a node, and another token with the same rules.
	listen := func(subjects []string, opts ...nats.Option) (*spy, *nats.Conn) {
		s := &spy{}
		nc, err := nats.Connect(busURL(s1), append(opts, nats.ErrorHandler(s.failure))...)
		must.NoError(t, err)
		t.Cleanup(nc.Close)
		for _, subject := range subjects {
			_, err := nc.Subscribe(subject, s.message)
			must.NoError(t, err) // the refusal comes from the server, later
		}
		must.NoError(t, nc.Flush())
		return s, nc
	}
	everyInbox := []string{"_INBOX.>", "_INBOX.*", "_INBOX.*.*", inboxA + ".>", ">"}
	nodeSpy, _ := listen(everyInbox, nats.UserInfo(node.ID, node.SecretID))
	tokenSpy, _ := listen(everyInbox, nats.Token(tokenB.SecretID),
		nats.CustomInboxPrefix(bus.InboxPrefix+"."+tokenB.AccessorID))

	// Replies of the server's own requests: a value of the Consul KV.
	_, err = kv.Put(t.Context(), "probe.key", []byte("TOP-SECRET-VALUE"))
	must.NoError(t, err)
	e, err := kv.Get(t.Context(), "probe.key")
	must.NoError(t, err)
	must.Eq(t, "TOP-SECRET-VALUE", string(e.Value()))
	w, err := kv.Watch(t.Context(), "probe.>")
	must.NoError(t, err)
	<-w.Updates()
	must.NoError(t, w.Stop())

	// A token gets the reply of its own request under its own prefix.
	a, err := nats.Connect(busURL(s1), nats.Token(tokenA.SecretID), nats.CustomInboxPrefix(inboxA))
	must.NoError(t, err)
	defer a.Close()
	reply, err := a.Request("svc.echo", []byte("ping"), 5*time.Second)
	must.NoError(t, err)
	must.Eq(t, "REPLY-FOR-A", string(reply.Data))

	// With the default inbox of NATS the reply does not arrive: that inbox
	// is nobody's.
	plain, err := nats.Connect(busURL(s1), nats.Token(tokenA.SecretID))
	must.NoError(t, err)
	defer plain.Close()
	_, err = plain.Request("svc.echo", []byte("ping"), time.Second)
	must.Error(t, err)

	time.Sleep(500 * time.Millisecond)
	for name, s := range map[string]*spy{"node": nodeSpy, "other token": tokenSpy} {
		seen, errs := s.result()
		for _, m := range seen {
			if strings.Contains(m, "TOP-SECRET-VALUE") || strings.Contains(m, "REPLY-FOR-A") || strings.HasPrefix(m, "_INBOX.") {
				t.Errorf("the %s read a reply of another connection: %.120s", name, m)
			}
		}
		// Every subscription into the inboxes was refused by the server.
		refused := 0
		for _, e := range errs {
			if strings.Contains(strings.ToLower(e), "permissions violation") {
				refused++
			}
		}
		must.GreaterEq(t, len(everyInbox), refused, must.Sprintf("%s: subscriptions refused, errors: %v", name, errs))
	}
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/nats-io/nats.go"
	"github.com/shoenig/test/must"
)

// TestBusServer_AnonymousHasNoInbox checks that the connections without a
// token, which all are the anonymous token, do not share an inbox: one of
// them would read the replies of the requests of the others. A token keeps
// its own inbox.
func TestBusServer_AnonymousHasNoInbox(t *testing.T) {
	ci.Parallel(t)

	s1, _, cleanup := TestACLServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	st := s1.fsm.State()

	rules := `bus {
  subscribe = ["svc.>"]
  publish   = ["svc.>"]
}`
	anonymous := mock.ACLPolicy()
	anonymous.Name = "anonymous"
	anonymous.Rules = rules
	anonymous.SetHash()
	named := mock.ACLPolicy()
	named.Name = "svc-user"
	named.Rules = rules
	named.SetHash()
	must.NoError(t, st.UpsertACLPolicies(structs.MsgTypeTestSetup, 100, []*structs.ACLPolicy{anonymous, named}))
	token := mock.ACLToken()
	token.Policies = []string{named.Name}
	token.SetHash()
	must.NoError(t, st.UpsertACLTokens(structs.MsgTypeTestSetup, 101, []*structs.ACLToken{token}))

	_, _, _, inbox, err := s1.busIdentity("")
	must.NoError(t, err)
	must.Eq(t, "", inbox, must.Sprint("the inbox of the anonymous connections"))

	srv, err := s1.bus.Connect()
	must.NoError(t, err)
	defer srv.Close()
	_, err = srv.Subscribe("svc.echo", func(m *nats.Msg) { _ = m.Respond([]byte("REPLY-FOR-THE-REQUESTER")) })
	must.NoError(t, err)
	must.NoError(t, srv.Flush())

	// An anonymous listener on the inbox the anonymous connections had.
	shared := bus.InboxPrefix + ".anonymous"
	listener := &spy{}
	lc, err := nats.Connect(busURL(s1), nats.ErrorHandler(listener.failure))
	must.NoError(t, err)
	defer lc.Close()
	_, err = lc.Subscribe(shared+".>", listener.message)
	must.NoError(t, err)
	must.NoError(t, lc.Flush())

	// Another anonymous connection asks; its reply must not reach the
	// listener (nor itself: it has no inbox).
	asker, err := nats.Connect(busURL(s1), nats.CustomInboxPrefix(shared))
	must.NoError(t, err)
	defer asker.Close()
	_, err = asker.Request("svc.echo", []byte("ping"), time.Second)
	must.Error(t, err)

	// A token gets the reply of its request in its own inbox.
	user, err := nats.Connect(busURL(s1), nats.Token(token.SecretID),
		nats.CustomInboxPrefix(bus.InboxPrefix+"."+token.AccessorID))
	must.NoError(t, err)
	defer user.Close()
	reply, err := user.Request("svc.echo", []byte("ping"), 5*time.Second)
	must.NoError(t, err)
	must.Eq(t, "REPLY-FOR-THE-REQUESTER", string(reply.Data))

	// The anonymous connections still use the subjects of their rule.
	got, err := lc.SubscribeSync("svc.news")
	must.NoError(t, err)
	must.NoError(t, lc.Flush())
	must.NoError(t, asker.Publish("svc.news", []byte("hello")))
	m, err := got.NextMsg(5 * time.Second)
	must.NoError(t, err)
	must.Eq(t, "hello", string(m.Data))

	seen, errs := listener.result()
	must.SliceEmpty(t, seen, must.Sprint("an anonymous connection read the reply of another one"))
	refused := false
	for _, e := range errs {
		if strings.Contains(e, "Subscription to \""+shared+".>\"") {
			refused = true
		}
	}
	must.True(t, refused, must.Sprintf("the subscription to the shared inbox was not refused: %v", errs))
}

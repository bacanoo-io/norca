// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"
)

// TestBusServer_BusRuleInternalSubjects checks that a bus rule stays outside
// JetStream and the subjects of the servers and nodes: the bucket of the
// Consul KV ($KV), the JetStream API and its internals ($JS, $JSC, $NRG),
// the deliveries to the leaf readers (_R_) and the telemetry of the nodes.
// A management token still reaches them.
func TestBusServer_BusRuleInternalSubjects(t *testing.T) {
	ci.Parallel(t)

	s1, root, cleanup := TestACLServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	st := s1.fsm.State()

	internal := []string{
		"$KV.consul-kv.>", "$KV.>", "$JS.API.>", "$JS.ACK.>", "$JSC.>", "$NRG.>", "_R_.>",
		"norca.telemetry.>", "norca.telemetry.global.*",
		// The services every account imports from the NATS system account,
		// the inboxes of the servers' own connections and the subject the
		// servers store the pack push notices on.
		"$SYS.REQ.USER.INFO", "$SYS.>", bus.InternalInboxPrefix + ".>", "norca." + bus.PushStoredSubject,
	}

	// The subjects a connection gets from the rule.
	grant := &acl.BusGrant{
		Subscribe: append([]string{"app.>"}, internal...),
		Publish:   append([]string{"app.>", "norca.packs.pushed"}, internal...),
	}
	publish, subscribe, dropped := s1.busGrantSubjects(grant, &acl.CDCGrant{})
	must.Eq(t, []string{"app.>"}, subscribe, must.Sprint("subscribe subjects of the bus rule"))
	must.Eq(t, []string{"app.>", "norca.packs.pushed"}, publish, must.Sprint("publish subjects of the bus rule"))
	must.Eq(t, 2*len(internal), dropped)

	// The same on a connection.
	policy := mock.ACLPolicy()
	policy.Name = "bus-internal"
	policy.Rules = `bus {
  subscribe = ["app.>", "$KV.consul-kv.>", "_R_.>", "$JS.API.>"]
  publish   = ["app.>", "$KV.consul-kv.>", "norca.telemetry.>"]
}`
	policy.SetHash()
	must.NoError(t, st.UpsertACLPolicies(structs.MsgTypeTestSetup, 100, []*structs.ACLPolicy{policy}))
	token := mock.ACLToken()
	token.Policies = []string{policy.Name}
	token.SetHash()
	must.NoError(t, st.UpsertACLTokens(structs.MsgTypeTestSetup, 101, []*structs.ACLToken{token}))

	connect := func(secret, accessor string) (*spy, *nats.Conn) {
		s := &spy{}
		nc, err := nats.Connect(busURL(s1), nats.Token(secret), nats.ErrorHandler(s.failure),
			nats.CustomInboxPrefix(bus.InboxPrefix+"."+accessor))
		must.NoError(t, err)
		t.Cleanup(nc.Close)
		for _, subject := range []string{"app.>", "$KV.consul-kv.>", "_R_.>", "$JS.API.>"} {
			_, err := nc.Subscribe(subject, s.message)
			must.NoError(t, err) // the refusal comes from the server, later
		}
		must.NoError(t, nc.Flush())
		return s, nc
	}
	userSpy, userConn := connect(token.SecretID, token.AccessorID)
	rootSpy, _ := connect(root.SecretID, root.AccessorID)

	// The servers write a key of the Consul KV.
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
	_, err = kv.Put(t.Context(), "probe.key", []byte("SEALED-VALUE"))
	must.NoError(t, err)

	// The connection writes the bucket and reports a usage in the name of a
	// node; its own subject works.
	must.NoError(t, userConn.Publish("$KV.consul-kv.forged.key", []byte("FORGED-VALUE")))
	must.NoError(t, userConn.Publish(structs.BusTelemetrySubject("global", "some-node"), []byte("{}")))
	must.NoError(t, userConn.Publish("app.hello", []byte("APP-MESSAGE")))
	must.NoError(t, userConn.Flush())

	contains := func(list []string, part string) bool {
		for _, m := range list {
			if strings.Contains(m, part) {
				return true
			}
		}
		return false
	}
	waitBus(t, func() (bool, error) {
		seen, _ := rootSpy.result()
		return contains(seen, "SEALED-VALUE") && contains(seen, "APP-MESSAGE"), nil
	})
	waitBus(t, func() (bool, error) {
		seen, _ := userSpy.result()
		return contains(seen, "APP-MESSAGE"), nil
	})
	time.Sleep(500 * time.Millisecond)

	seen, errs := userSpy.result()
	for _, m := range seen {
		if !strings.HasPrefix(m, "app.") {
			t.Errorf("the bus rule received %.120s", m)
		}
	}
	for _, subject := range []string{`"$KV.consul-kv.>"`, `"_R_.>"`, `"$JS.API.>"`} {
		must.True(t, contains(errs, "Subscription to "+subject), must.Sprintf("%s not refused: %v", subject, errs))
	}
	must.True(t, contains(errs, `Publish to "$KV.consul-kv.forged.key"`), must.Sprintf("errors: %v", errs))
	must.True(t, contains(errs, `Publish to "norca.telemetry.global.some-node"`), must.Sprintf("errors: %v", errs))

	// The forged key is not in the bucket.
	_, err = kv.Get(t.Context(), "forged.key")
	must.ErrorIs(t, err, jetstream.ErrKeyNotFound)

	// The management token is not restricted.
	_, rootErrs := rootSpy.result()
	must.SliceEmpty(t, rootErrs)
}

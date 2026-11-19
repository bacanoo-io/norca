// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package bus

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/hashicorp/go-hclog"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"
)

// testAuthz grants by credentials: node "n1" with secret "s1" may publish
// its telemetry subject, token "admin" everything, token "reader" may
// subscribe to events. Changing it takes effect on Revalidate.
type testAuthz struct {
	l      sync.Mutex
	tokens map[string]*Grant
	reader func() (*Grant, error)
}

func newTestAuthz() *testAuthz {
	return &testAuthz{tokens: map[string]*Grant{
		"admin":  {Kind: KindToken, Name: "admin", All: true},
		"reader": {Kind: KindToken, Name: "reader", Subscribe: []string{"events.>"}},
	}}
}

func (a *testAuthz) authorize(c Credentials) (*Grant, error) {
	a.l.Lock()
	defer a.l.Unlock()
	if c.Username == "n1" && c.Password == "s1" {
		return &Grant{Kind: KindNode, Name: "n1", Publish: []string{"telemetry.global.n1"}}, nil
	}
	if g, ok := a.tokens[c.Token]; ok {
		return g, nil
	}
	if c.Token == "cdc" && a.reader != nil {
		return a.reader()
	}
	return nil, errors.New("unknown credentials")
}

// withReader authenticates the token "cdc" with grant, called for each
// connection.
func (a *testAuthz) withReader(grant func() (*Grant, error)) {
	a.l.Lock()
	defer a.l.Unlock()
	a.reader = grant
}

func (a *testAuthz) set(token string, g *Grant) {
	a.l.Lock()
	defer a.l.Unlock()
	if g == nil {
		delete(a.tokens, token)
	} else {
		a.tokens[token] = g
	}
}

func testConfig(t *testing.T, name string) *Config {
	ports := ci.PortAllocator.Grab(2)
	c := DefaultConfig()
	c.Enabled = true
	c.ServerName = name
	c.Region = "global"
	c.BindAddr = "127.0.0.1"
	c.Port = ports[0]
	c.ClusterPort = ports[1]
	// Windows may keep a JetStream file open for a moment after the
	// shutdown, which fails the cleanup of t.TempDir: removed best effort.
	store, err := os.MkdirTemp("", "norca-bus-test")
	must.NoError(t, err)
	t.Cleanup(func() {
		_ = os.RemoveAll(store)
		_ = os.RemoveAll(store + ".standalone")
	})
	c.StoreDir = store
	c.MaxMemory = 32 << 20
	c.MaxStorage = 128 << 20
	c.ConsulKVMaxBytes = 16 << 20
	c.Events.MaxBytes = 16 << 20
	return c
}

func startServer(t *testing.T, c *Config, authz Authorizer) *Server {
	s, err := New(c, hclog.NewNullLogger(), authz)
	must.NoError(t, err)
	t.Cleanup(s.Shutdown)
	return s
}

func TestBus_SingleServer(t *testing.T) {
	ci.Parallel(t)
	authz := newTestAuthz()
	s := startServer(t, testConfig(t, "s1"), authz.authorize)

	// A single server is a JetStream cluster of one.
	testutil.WaitForResult(func() (bool, error) {
		return s.JetStreamReady(), errors.New("no JetStream meta leader")
	}, func(err error) { t.Fatal(err) })

	nc, err := s.Connect()
	must.NoError(t, err)
	defer nc.Close()
	js, err := jetstream.New(nc)
	must.NoError(t, err)
	_, err = js.CreateStream(t.Context(), jetstream.StreamConfig{Name: "TEST", Subjects: []string{"test.>"}})
	must.NoError(t, err)
	_, err = js.Publish(t.Context(), "test.a", []byte("x"))
	must.NoError(t, err)

	st, err := s.Status()
	must.NoError(t, err)
	must.True(t, st.Enabled)
	must.Eq(t, "s1", st.Server)
	must.Eq(t, "global", st.Cluster)
	must.NotNil(t, st.JetStream)
	must.Eq(t, "s1", st.JetStream.MetaLeader)
	must.Eq(t, 1, st.JetStream.Streams)
	must.Len(t, 1, st.JetStream.StreamDetails)
	sd := st.JetStream.StreamDetails[0]
	must.Eq(t, "TEST", sd.Name)
	must.Eq(t, []string{"test.>"}, sd.Subjects)
	must.Eq(t, uint64(1), sd.Messages)
	must.Eq(t, 1, sd.Replicas)
	must.Eq(t, "s1", sd.Leader)
	must.Eq(t, 1, st.Connections.Internal)

	// The sample has the totals of the streams and no routes.
	sample, err := s.Sample()
	must.NoError(t, err)
	must.True(t, sample.JetStream)
	must.Eq(t, uint64(1), sample.Messages)
	must.Eq(t, sd.Bytes, sample.Bytes)
	must.Positive(t, sample.Bytes)
	must.Eq(t, 0, sample.RouteServers)
}

func TestBus_Authentication(t *testing.T) {
	ci.Parallel(t)
	authz := newTestAuthz()
	s := startServer(t, testConfig(t, "s1"), authz.authorize)
	url := s.ClientURL()

	// Unknown credentials and a wrong node secret are refused.
	_, err := nats.Connect(url, nats.Token("nope"))
	must.Error(t, err)
	_, err = nats.Connect(url, nats.UserInfo("n1", "wrong"))
	must.Error(t, err)
	_, err = nats.Connect(url)
	must.Error(t, err)

	// The node may publish on its own subject only.
	errs := make(chan error, 4)
	node, err := nats.Connect(url, nats.UserInfo("n1", "s1"),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) { errs <- err }))
	must.NoError(t, err)
	defer node.Close()

	admin, err := nats.Connect(url, nats.Token("admin"))
	must.NoError(t, err)
	defer admin.Close()
	sub, err := admin.SubscribeSync("telemetry.>")
	must.NoError(t, err)
	must.NoError(t, admin.Flush())

	must.NoError(t, node.Publish("telemetry.global.n1", []byte("ok")))
	must.NoError(t, node.Publish("telemetry.global.n2", []byte("spoofed")))
	must.NoError(t, node.Flush())

	msg, err := sub.NextMsg(5 * time.Second)
	must.NoError(t, err)
	must.Eq(t, "telemetry.global.n1", msg.Subject)
	_, err = sub.NextMsg(300 * time.Millisecond)
	must.ErrorIs(t, err, nats.ErrTimeout)
	select {
	case err := <-errs:
		must.ErrorContains(t, err, "Permissions Violation")
	case <-time.After(5 * time.Second):
		t.Fatal("no permission violation for the spoofed subject")
	}

	// A node may not subscribe to anything but its own inbox.
	_, err = node.SubscribeSync("telemetry.>")
	must.NoError(t, err) // asynchronous: the violation arrives as an error
	select {
	case err := <-errs:
		must.ErrorContains(t, err, "Permissions Violation")
	case <-time.After(5 * time.Second):
		t.Fatal("no permission violation for the subscription")
	}

	st, err := s.Status()
	must.NoError(t, err)
	must.Eq(t, 1, st.Connections.Nodes)
	must.Eq(t, 1, st.Connections.Tokens)
}

func TestBus_Revalidate(t *testing.T) {
	ci.Parallel(t)
	authz := newTestAuthz()
	s := startServer(t, testConfig(t, "s1"), authz.authorize)

	closed := make(chan struct{})
	reader, err := nats.Connect(s.ClientURL(), nats.Token("reader"), nats.NoReconnect(),
		nats.ClosedHandler(func(*nats.Conn) { close(closed) }))
	must.NoError(t, err)
	defer reader.Close()
	admin, err := nats.Connect(s.ClientURL(), nats.Token("admin"), nats.NoReconnect())
	must.NoError(t, err)
	defer admin.Close()

	// Nothing changed: both stay.
	s.Revalidate()
	must.True(t, reader.IsConnected())

	// The reader's token is revoked: its connection is closed, the admin's
	// stays.
	authz.set("reader", nil)
	s.Revalidate()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the connection of the revoked token was not closed")
	}
	must.True(t, admin.IsConnected())

	// A changed grant closes the connection too, so it reconnects with the
	// new one.
	authz.set("admin", &Grant{Kind: KindToken, Name: "admin", Subscribe: []string{"x"}})
	s.Revalidate()
	testutil.WaitForResult(func() (bool, error) {
		return admin.IsClosed(), errors.New("admin connection still open")
	}, func(err error) { t.Fatal(err) })
}

func TestBus_Cluster(t *testing.T) {
	ci.Parallel(t)
	authz := newTestAuthz()

	var servers []*Server
	var clusterAddrs []string
	for i := range 3 {
		c := testConfig(t, fmt.Sprintf("s%d", i+1))
		c.Clustered = true
		clusterAddrs = append(clusterAddrs, net.JoinHostPort(c.BindAddr, strconv.Itoa(c.ClusterPort)))
		servers = append(servers, startServer(t, c, authz.authorize))
	}
	for i, s := range servers {
		var routes []string
		for j, a := range clusterAddrs {
			if j != i {
				routes = append(routes, a)
			}
		}
		must.NoError(t, s.SetRoutes(routes))
		// An unchanged list is not reloaded.
		must.NoError(t, s.SetRoutes(routes))
	}

	testutil.WaitForResult(func() (bool, error) {
		for _, s := range servers {
			st, err := s.Status()
			if err != nil {
				return false, err
			}
			if len(st.Routes) < 2 {
				return false, fmt.Errorf("%s has %d routes", st.Server, len(st.Routes))
			}
			if st.JetStream == nil || st.JetStream.MetaLeader == "" || st.JetStream.ClusterSize != 3 {
				return false, fmt.Errorf("%s: JetStream %+v", st.Server, st.JetStream)
			}
		}
		return true, nil
	}, func(err error) { t.Fatal(err) })

	// The routes are counted by server, not by the connections of the pool.
	testutil.WaitForResult(func() (bool, error) {
		for i, s := range servers {
			sample, err := s.Sample()
			if err != nil {
				return false, err
			}
			if sample.RouteServers != 2 {
				return false, fmt.Errorf("s%d has routes to %d servers", i+1, sample.RouteServers)
			}
		}
		return true, nil
	}, func(err error) { t.Fatal(err) })

	// A replicated stream written through one server is read through
	// another.
	nc1, err := servers[0].Connect()
	must.NoError(t, err)
	defer nc1.Close()
	js1, err := jetstream.New(nc1)
	must.NoError(t, err)
	createStream(t, js1, jetstream.StreamConfig{Name: "R3", Subjects: []string{"r3.>"}, Replicas: 3})
	// The new stream may not have a leader yet.
	testutil.WaitForResult(func() (bool, error) {
		_, err := js1.Publish(t.Context(), "r3.a", []byte("hello"))
		return err == nil, err
	}, func(err error) { t.Fatal(err) })

	nc3, err := servers[2].Connect()
	must.NoError(t, err)
	defer nc3.Close()
	js3, err := jetstream.New(nc3)
	must.NoError(t, err)
	stream, err := js3.Stream(t.Context(), "R3")
	must.NoError(t, err)
	m, err := stream.GetLastMsgForSubject(t.Context(), "r3.a")
	must.NoError(t, err)
	must.Eq(t, "hello", string(m.Data))
}

// TestBus_StandaloneToCluster covers a region growing from one server to
// two: the standalone bus restarts clustered, keeps its old store aside,
// and its in-process connection reconnects to the new server.
func TestBus_StandaloneToCluster(t *testing.T) {
	ci.Parallel(t)
	authz := newTestAuthz()

	c1 := testConfig(t, "s1")
	s1 := startServer(t, c1, authz.authorize)
	must.False(t, s1.Clustered())
	must.True(t, s1.JetStreamReady())

	reconnected := make(chan struct{}, 1)
	nc, err := s1.Connect(nats.ReconnectWait(100*time.Millisecond),
		nats.ReconnectHandler(func(*nats.Conn) { reconnected <- struct{}{} }))
	must.NoError(t, err)
	defer nc.Close()
	js, err := jetstream.New(nc)
	must.NoError(t, err)
	_, err = js.CreateStream(t.Context(), jetstream.StreamConfig{Name: "OLD", Subjects: []string{"old.>"}})
	must.NoError(t, err)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "kvtest"})
	must.NoError(t, err)
	_, err = kv.Put(t.Context(), "a.b", []byte("v1"))
	must.NoError(t, err)
	_, err = kv.Put(t.Context(), "gone", []byte("x"))
	must.NoError(t, err)
	must.NoError(t, kv.Delete(t.Context(), "gone"))

	// No routes: stays standalone.
	must.NoError(t, s1.SetRoutes(nil))
	must.False(t, s1.Clustered())

	c2 := testConfig(t, "s2")
	c2.Clustered = true
	s2 := startServer(t, c2, authz.authorize)
	addr1 := net.JoinHostPort(c1.BindAddr, strconv.Itoa(c1.ClusterPort))
	addr2 := net.JoinHostPort(c2.BindAddr, strconv.Itoa(c2.ClusterPort))
	must.NoError(t, s2.SetRoutes([]string{addr1}))
	must.NoError(t, s1.SetRoutes([]string{addr2}))
	must.True(t, s1.Clustered())
	_, err = os.Stat(s1.StandaloneStoreDir())
	must.NoError(t, err)

	// The moment JetStream is reported ready, its API answers (MUK-23): a
	// meta leader shows as soon as it is elected but answers only after it
	// processed its first entries, and a request in between got no answer
	// from any server.
	probe, err := s1.Connect()
	must.NoError(t, err)
	defer probe.Close()
	for deadline := time.Now().Add(20 * time.Second); !s1.JetStreamReady(); {
		must.True(t, time.Now().Before(deadline), must.Sprint("JetStream never ready"))
		time.Sleep(time.Millisecond)
	}
	_, err = probe.Request("$JS.API.STREAM.INFO.OLD", nil, 2*time.Second)
	must.NoError(t, err, must.Sprint("the first API request after JetStreamReady was not answered"))

	select {
	case <-reconnected:
	case <-time.After(10 * time.Second):
		t.Fatal("the in-process connection did not reconnect")
	}
	testutil.WaitForResult(func() (bool, error) {
		st, err := s1.Status()
		if err != nil {
			return false, err
		}
		if st.JetStream == nil || st.JetStream.MetaLeader == "" || st.JetStream.ClusterSize != 2 {
			return false, fmt.Errorf("JetStream %+v", st.JetStream)
		}
		return true, nil
	}, func(err error) { t.Fatal(err) })

	// The streams of the standalone bus are not carried over; new ones work.
	// A meta leader answers the API only after it processed its first
	// entries (MUK-23): JetStreamReady waits for an answer, after which the
	// first request is answered, whichever server leads.
	testutil.WaitForResult(func() (bool, error) {
		return s1.JetStreamReady(), errors.New("JetStream API not ready")
	}, func(err error) { t.Fatal(err) })
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err = js.Stream(ctx, "OLD")
	must.ErrorIs(t, err, jetstream.ErrStreamNotFound)
	createStream(t, js, jetstream.StreamConfig{Name: "NEW", Subjects: []string{"new.>"}, Replicas: 2})

	// The key-value buckets of the standalone store can be read from the
	// store kept aside: live keys only; a missing bucket has no keys.
	entries, err := ReadStoreKV(t.Context(), s1.StandaloneStoreDir(), "kvtest", c1.MaxStorage, hclog.NewNullLogger())
	must.NoError(t, err)
	must.Eq(t, []KVEntry{{Key: "a.b", Value: []byte("v1")}}, entries)
	entries, err = ReadStoreKV(t.Context(), s1.StandaloneStoreDir(), "missing", c1.MaxStorage, hclog.NewNullLogger())
	must.NoError(t, err)
	must.Nil(t, entries)
}

// createStream creates a replicated stream, retrying while JetStream has a
// meta leader but not every peer reported in yet ("no suitable peers").
func createStream(t *testing.T, js jetstream.JetStream, cfg jetstream.StreamConfig) {
	t.Helper()
	testutil.WaitForResult(func() (bool, error) {
		_, err := js.CreateStream(t.Context(), cfg)
		return err == nil, err
	}, func(err error) { t.Fatal(err) })
}

func TestSubjectIsSubsetMatch(t *testing.T) {
	tests := []struct {
		subject, pattern string
		expected         bool
	}{
		{"norca.eu.games.Job", "norca.eu.*.*", true},
		{"norca.eu.games.*", "norca.eu.*.*", true},
		{"norca.eu.>", "norca.eu.*.*", false},
		{"norca.eu.*.*", "norca.>", true},
		{"norca.eu.games.ConsulKV.*", "norca.eu.*.ConsulKV.*", true},
		{"norca.eu.games.ConsulKV.*", "norca.eu.*.*", false},
		{"norca.eu.*.Job", "norca.eu.games.>", false},
		{"norca.eu", "norca.eu.>", false},
		{"norca.eu.games.Job", "norca.eu.games.Job", true},
	}
	for _, tt := range tests {
		if got := SubjectIsSubsetMatch(tt.subject, tt.pattern); got != tt.expected {
			t.Errorf("SubjectIsSubsetMatch(%q, %q) = %v, want %v", tt.subject, tt.pattern, got, tt.expected)
		}
	}
}

func TestSubjectIntersect(t *testing.T) {
	ci.Parallel(t)
	tests := []struct {
		a, b     string
		expected string
		ok       bool
	}{
		{"norca.>", "norca.eu.*.*", "norca.eu.*.*", true},
		{"norca.eu.*.*", "norca.>", "norca.eu.*.*", true},
		{"norca.*.games.>", "norca.eu.*.*", "norca.eu.games.*", true},
		{"norca.*.games.>", "norca.eu.*.ConsulKV.*", "norca.eu.games.ConsulKV.*", true},
		{"norca.eu.games.Job", "norca.eu.*.*", "norca.eu.games.Job", true},
		{"norca.eu.*.Job", "norca.eu.games.*", "norca.eu.games.Job", true},
		{">", "norca.eu.*.*", "norca.eu.*.*", true},
		{"norca.>", "norca.>", "norca.>", true},
		{"norca.us.>", "norca.eu.*.*", "", false},
		{"norca.eu.*", "norca.eu.*.*", "", false},
		{"norca.eu.*.*", "norca.eu.*.ConsulKV.*", "", false},
		{"norca.eu.>", "norca.eu", "", false},
		{"norca.eu", "norca.eu.>", "", false},
	}
	for _, tt := range tests {
		got, ok := SubjectIntersect(tt.a, tt.b)
		if got != tt.expected || ok != tt.ok {
			t.Errorf("SubjectIntersect(%q, %q) = %q, %v, want %q, %v", tt.a, tt.b, got, ok, tt.expected, tt.ok)
		}
	}
}

// A connection that the server does not list yet is not forgotten (review
// finding SRV-23): a leaf node is recorded during its CONNECT and listed
// only after it. Forgotten, it would never be checked again.
func TestBus_Revalidate_NotListedYet(t *testing.T) {
	ci.Parallel(t)
	authz := newTestAuthz()
	s := startServer(t, testConfig(t, "s1"), authz.authorize)

	// A connection as clientAuth.Check records it, with an ID the server
	// does not list.
	const id = uint64(1) << 40
	record := func(at time.Time) {
		grant, err := authz.authorize(Credentials{Token: "reader"})
		must.NoError(t, err)
		s.l.Lock()
		s.clients[id] = &client{creds: Credentials{Token: "reader"}, grant: grant, at: at}
		s.l.Unlock()
	}
	recorded := func() bool {
		s.l.Lock()
		defer s.l.Unlock()
		_, ok := s.clients[id]
		return ok
	}

	record(time.Now())
	s.Revalidate()
	must.True(t, recorded(), must.Sprint("a new connection that is not listed yet"))

	// Its rights change while it can't be closed yet: it stays known.
	authz.set("reader", nil)
	s.Revalidate()
	must.True(t, recorded(), must.Sprint("a new connection whose rights changed"))
	authz.set("reader", &Grant{Kind: KindToken, Name: "reader", Subscribe: []string{"events.>"}})

	// A connection that is not listed after the grace time is closed.
	record(time.Now().Add(-2 * connectGrace))
	s.Revalidate()
	must.False(t, recorded())
}

// nats-server authenticates the connections again during a reload of its
// options, also one that has not sent its CONNECT yet. The authorizer may
// ask for a reader account then (an anonymous policy with a cdc rule and no
// bus rule): the reload must not wait for itself. Every compaction of the
// reader accounts reloads (review finding CON-5).
func TestBus_ReloadAuthenticatesReader(t *testing.T) {
	ci.Parallel(t)
	c := testConfig(t, "s1")
	c.Clustered = true
	var (
		l   sync.Mutex
		bus *Server
	)
	s := startServer(t, c, func(Credentials) (*Grant, error) {
		l.Lock()
		s := bus
		l.Unlock()
		name, err := s.ReaderAccount(readerTestSpec("anonymous"))
		if err != nil {
			return nil, err
		}
		return &Grant{Kind: KindToken, Name: "anonymous", All: true, Account: name}, nil
	})
	l.Lock()
	bus = s
	l.Unlock()

	// A connection that sends no CONNECT: the server lists it until its
	// authentication times out.
	conn, err := net.Dial("tcp", net.JoinHostPort(c.BindAddr, strconv.Itoa(c.Port)))
	must.NoError(t, err)
	defer conn.Close()
	testutil.WaitForResult(func() (bool, error) {
		n := s.server().NumClients()
		return n == 1, fmt.Errorf("%d clients", n)
	}, func(err error) { t.Fatal(err) })

	done := make(chan error, 1)
	go func() { done <- s.SetRoutes([]string{"127.0.0.1:1"}) }()
	select {
	case err := <-done:
		must.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("the reload of the routes waits for itself")
	}
}

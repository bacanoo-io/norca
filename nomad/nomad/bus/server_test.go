// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package bus

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/testutil"
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
	return nil, errors.New("unknown credentials")
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
	t.Cleanup(func() { _ = os.RemoveAll(store) })
	c.StoreDir = store
	c.MaxMemory = 32 << 20
	c.MaxStorage = 128 << 20
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
	_, err = js.Stream(t.Context(), "OLD")
	must.ErrorIs(t, err, jetstream.ErrStreamNotFound)
	createStream(t, js, jetstream.StreamConfig{Name: "NEW", Subjects: []string{"new.>"}, Replicas: 2})
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

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/nomad/bus"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/hashicorp/nomad/testutil"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"
)

// enableBus returns a configuration callback that turns the bus on, on free
// ports. The JetStream store is a directory of its own, removed best effort:
// Windows may keep a file of it open for a moment after the shutdown, which
// would fail the cleanup of the server's t.TempDir.
func enableBus(t *testing.T) func(*Config) {
	return func(c *Config) {
		store, err := os.MkdirTemp("", "norca-bus-test")
		must.NoError(t, err)
		t.Cleanup(func() {
			_ = os.RemoveAll(store)
			_ = os.RemoveAll(store + ".standalone")
		})
		ports := ci.PortAllocator.Grab(2)
		c.Bus.Enabled = true
		c.Bus.Port = ports[0]
		c.Bus.ClusterPort = ports[1]
		c.Bus.StoreDir = store
		c.Bus.MaxMemory = 32 << 20
		c.Bus.MaxStorage = 128 << 20
		c.Bus.Events.MaxBytes = 16 << 20
	}
}

// waitBus waits up to 30 seconds for fn: JetStream needs longer than the
// default wait to elect leaders under the race detector and parallel tests.
func waitBus(t *testing.T, fn func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		ok, err := fn()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func busURL(s *Server) string {
	return s.bus.ClientURL()
}

func TestBusServer_Authentication(t *testing.T) {
	ci.Parallel(t)

	s1, root, cleanup := TestACLServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	st := s1.fsm.State()

	// A node logs in with its ID and secret and may publish its telemetry.
	node := mock.Node()
	must.NoError(t, st.UpsertNode(structs.MsgTypeTestSetup, 100, node))
	nc, err := nats.Connect(busURL(s1), nats.UserInfo(node.ID, node.SecretID))
	must.NoError(t, err)
	defer nc.Close()
	_, err = nats.Connect(busURL(s1), nats.UserInfo(node.ID, "wrong"))
	must.Error(t, err)

	// The node may use the key-value bucket of the Consul KV, which the
	// leader creates, and no other stream.
	nodeJS, err := jetstream.New(nc)
	must.NoError(t, err)
	waitBus(t, func() (bool, error) {
		kv, err := nodeJS.KeyValue(t.Context(), bus.ConsulKVBucket)
		if err != nil {
			return false, err
		}
		if _, err := kv.Put(t.Context(), "app.config", []byte("v")); err != nil {
			return false, err
		}
		e, err := kv.Get(t.Context(), "app.config")
		return err == nil && string(e.Value()) == "v", err
	})
	_, err = nodeJS.Stream(t.Context(), pushStreamName)
	must.Error(t, err)

	// A token needs a bus rule.
	policy := mock.ACLPolicy()
	policy.Name = "bus-events"
	policy.Rules = `bus { subscribe = ["nomad.*.>"] }`
	policy.SetHash()
	plain := mock.ACLPolicy()
	must.NoError(t, st.UpsertACLPolicies(structs.MsgTypeTestSetup, 101, []*structs.ACLPolicy{policy, plain}))
	busToken := mock.ACLToken()
	busToken.Policies = []string{policy.Name}
	plainToken := mock.ACLToken()
	plainToken.Policies = []string{plain.Name}
	must.NoError(t, st.UpsertACLTokens(structs.MsgTypeTestSetup, 102, []*structs.ACLToken{busToken, plainToken}))

	_, err = nats.Connect(busURL(s1), nats.Token(plainToken.SecretID))
	must.Error(t, err)
	_, err = nats.Connect(busURL(s1)) // anonymous, no bus rule
	must.Error(t, err)

	mgmt, err := nats.Connect(busURL(s1), nats.Token(root.SecretID))
	must.NoError(t, err)
	defer mgmt.Close()

	closed := make(chan struct{})
	consumer, err := nats.Connect(busURL(s1), nats.Token(busToken.SecretID), nats.NoReconnect(),
		nats.ClosedHandler(func(*nats.Conn) { close(closed) }))
	must.NoError(t, err)
	defer consumer.Close()
	sub, err := consumer.SubscribeSync("nomad.global.Job")
	must.NoError(t, err)
	must.NoError(t, consumer.Flush())
	must.NoError(t, mgmt.Publish("nomad.global.Job", []byte("event")))
	_, err = sub.NextMsg(5 * time.Second)
	must.NoError(t, err)

	// The status counts the connections.
	codec := rpcClient(t, s1)
	req := &structs.BusStatusRequest{QueryOptions: structs.QueryOptions{Region: "global", AuthToken: root.SecretID}}
	var resp structs.BusStatusResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Operator.BusStatus", req, &resp))
	must.NotNil(t, resp.Status)
	must.True(t, resp.Status.Enabled)
	must.False(t, resp.Status.Clustered)
	must.Eq(t, 1, resp.Status.Connections.Nodes)
	must.Eq(t, 2, resp.Status.Connections.Tokens)

	// The status needs operator read.
	req.AuthToken = busToken.SecretID
	err = msgpackrpc.CallWithCodec(codec, "Operator.BusStatus", req, &resp)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())

	// Deleting the token closes its connection.
	must.NoError(t, st.DeleteACLTokens(structs.MsgTypeTestSetup, 103, []string{busToken.AccessorID}))
	select {
	case <-closed:
	case <-time.After(3 * busRevalidateInterval):
		t.Fatal("the connection of the deleted token was not closed")
	}
	must.True(t, mgmt.IsConnected())
	must.True(t, nc.IsConnected())
}

func TestBusServer_Cluster(t *testing.T) {
	ci.Parallel(t)

	cb := func(c *Config) {
		enableBus(t)(c)
		c.BootstrapExpect = 3
	}
	s1, cleanup1 := TestServer(t, cb)
	defer cleanup1()
	s2, cleanup2 := TestServer(t, cb)
	defer cleanup2()
	s3, cleanup3 := TestServer(t, cb)
	defer cleanup3()
	servers := []*Server{s1, s2, s3}
	for _, s := range servers {
		must.True(t, s.bus.Clustered())
	}
	TestJoin(t, servers...)
	testutil.WaitForLeader(t, s1.RPC)

	// The routes follow Serf: every server reaches the other two and
	// JetStream elects a meta leader over the three. The meta group may
	// form with two servers first and take in the third a little later.
	start := time.Now()
	deadline := start.Add(60 * time.Second)
	for {
		err := func() error {
			for _, s := range servers {
				st, err := s.busStatus()
				if err != nil {
					return err
				}
				if len(st.Routes) < 2 {
					return fmt.Errorf("%s has %d routes", st.Server, len(st.Routes))
				}
				if st.JetStream == nil || st.JetStream.MetaLeader == "" || st.JetStream.ClusterSize != 3 {
					return fmt.Errorf("%s: JetStream %+v", st.Server, st.JetStream)
				}
			}
			return nil
		}()
		if err == nil {
			t.Logf("bus cluster formed after %s", time.Since(start))
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	// The status of another server is forwarded to it.
	codec := rpcClient(t, s1)
	req := &structs.BusStatusRequest{
		ServerID:     s3.LocalMember().Name,
		QueryOptions: structs.QueryOptions{Region: "global"},
	}
	var resp structs.BusStatusResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Operator.BusStatus", req, &resp))
	must.Eq(t, s3.LocalMember().Name, resp.Status.Server)
	must.True(t, resp.Status.Clustered)
}

func TestBusServer_Disabled(t *testing.T) {
	ci.Parallel(t)

	s1, cleanup := TestServer(t, nil)
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	must.Nil(t, s1.bus)
	_, ok := s1.LocalMember().Tags[busTag]
	must.False(t, ok)

	codec := rpcClient(t, s1)
	req := &structs.BusStatusRequest{QueryOptions: structs.QueryOptions{Region: "global"}}
	var resp structs.BusStatusResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Operator.BusStatus", req, &resp))
	must.False(t, resp.Status.Enabled)
}

// TestBusServer_TLSRoutes checks the TLS of the bus: routes need the
// certificate of a server of the region, clients need none.
func TestBusServer_TLSRoutes(t *testing.T) {
	ci.Parallel(t)

	const dir = "../helper/tlsutil/testdata/"
	tlsFor := func(cert string) *config.TLSConfig {
		return &config.TLSConfig{
			EnableRPC:            true,
			VerifyServerHostname: true,
			CAFile:               dir + "nomad-agent-ca.pem",
			CertFile:             dir + cert + ".pem",
			KeyFile:              dir + cert + "-key.pem",
		}
	}
	start := func(name, cert string) (*bus.Server, string) {
		clientTLS, clusterTLS, err := busTLS(tlsFor(cert), "global")
		must.NoError(t, err)
		cfg := bus.DefaultConfig()
		cfg.Enabled = true
		cfg.Clustered = true
		cfg.ServerName = name
		cfg.Region = "global"
		cfg.BindAddr = "127.0.0.1"
		// Windows may keep a JetStream file open for a moment after the
		// shutdown, which fails the cleanup of t.TempDir.
		store, err := os.MkdirTemp("", "norca-bus-test")
		must.NoError(t, err)
		t.Cleanup(func() { _ = os.RemoveAll(store) })
		cfg.StoreDir = store
		cfg.MaxMemory, cfg.MaxStorage = 32<<20, 128<<20
		cfg.Events.MaxBytes = 16 << 20
		cfg.ClientTLS, cfg.ClusterTLS = clientTLS, clusterTLS
		// A reserved port may still be taken by another test for a moment:
		// retried with new ports.
		for attempt := 1; ; attempt++ {
			ports := ci.PortAllocator.Grab(2)
			cfg.Port, cfg.ClusterPort = ports[0], ports[1]
			b, err := bus.New(cfg, testlog.HCLogger(t), func(c bus.Credentials) (*bus.Grant, error) {
				if c.Token == "ok" {
					return &bus.Grant{Kind: bus.KindToken, Name: "ok", All: true}, nil
				}
				return nil, errors.New("denied")
			})
			if err != nil && attempt < 3 {
				t.Logf("starting bus %s: %v; retrying with new ports", name, err)
				continue
			}
			must.NoError(t, err)
			t.Cleanup(b.Shutdown)
			return b, fmt.Sprintf("127.0.0.1:%d", ports[1])
		}
	}

	s1, addr1 := start("s1", "global-server-nomad")
	s2, addr2 := start("s2", "global-server-nomad")
	// A client certificate of the region, and a server certificate of
	// another CA, are not servers of this region.
	s3, addr3 := start("s3", "global-client-nomad")
	s4, addr4 := start("s4", "badRegion-server-bad")
	must.NoError(t, s1.SetRoutes([]string{addr2, addr3, addr4}))
	must.NoError(t, s2.SetRoutes([]string{addr1}))
	must.NoError(t, s3.SetRoutes([]string{addr1}))
	must.NoError(t, s4.SetRoutes([]string{addr1}))

	deadline := time.Now().Add(30 * time.Second)
	for {
		st, err := s1.Status()
		must.NoError(t, err)
		if st.JetStream != nil && st.JetStream.MetaLeader != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no meta leader: %+v", st.JetStream)
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Give the refused routes time to be tried.
	time.Sleep(2 * time.Second)
	st, err := s1.Status()
	must.NoError(t, err)
	for _, r := range st.Routes {
		must.Eq(t, "s2", r.Server, must.Sprint("unexpected route"))
	}
	must.SliceNotEmpty(t, st.Routes)

	// Clients connect over TLS without a certificate of their own.
	pool := x509.NewCertPool()
	ca, err := os.ReadFile(dir + "nomad-agent-ca.pem")
	must.NoError(t, err)
	must.True(t, pool.AppendCertsFromPEM(ca))
	tlsConf := &tls.Config{RootCAs: pool, ServerName: "server.global.nomad"}
	nc, err := nats.Connect(s1.ClientURL(), nats.Token("ok"), nats.Secure(tlsConf))
	must.NoError(t, err)
	nc.Close()
	_, err = nats.Connect(s1.ClientURL(), nats.Token("nope"), nats.Secure(tlsConf))
	must.Error(t, err)
}

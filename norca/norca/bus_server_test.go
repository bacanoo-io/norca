// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/norca/structs/config"
	"github.com/bacanoo-io/norca/norca/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
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
		c.Bus.ConsulKVMaxBytes = 16 << 20
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

	// The node has no access to the key-value bucket of the Consul KV,
	// which the leader creates (it is used through the ConsulKV endpoint
	// with the ACL of the request token), and to no other stream.
	mgmtKV, err := s1.bus.Connect()
	must.NoError(t, err)
	defer mgmtKV.Close()
	mgmtJS, err := jetstream.New(mgmtKV)
	must.NoError(t, err)
	waitBus(t, func() (bool, error) {
		kv, err := mgmtJS.KeyValue(t.Context(), bus.ConsulKVBucket)
		if err != nil {
			return false, err
		}
		_, err = kv.Put(t.Context(), "app.config", []byte("v"))
		return err == nil, err
	})
	nodeJS, err := jetstream.New(nc)
	must.NoError(t, err)
	_, err = nodeJS.KeyValue(t.Context(), bus.ConsulKVBucket)
	must.Error(t, err)
	// The subjects of the KV API are denied too.
	inbox, err := nc.SubscribeSync(nc.NewRespInbox())
	must.NoError(t, err)
	must.NoError(t, nc.PublishRequest("$JS.API.DIRECT.GET.KV_"+bus.ConsulKVBucket+".$KV."+bus.ConsulKVBucket+".app.config", inbox.Subject, nil))
	must.NoError(t, nc.PublishMsg(&nats.Msg{Subject: "$KV." + bus.ConsulKVBucket + ".app.config", Data: []byte("evil")}))
	must.NoError(t, nc.Flush())
	_, err = inbox.NextMsg(time.Second)
	must.ErrorIs(t, err, nats.ErrTimeout)
	must.ErrorContains(t, nc.LastError(), "Permissions Violation")
	kv, err := mgmtJS.KeyValue(t.Context(), bus.ConsulKVBucket)
	must.NoError(t, err)
	e, err := kv.Get(t.Context(), "app.config")
	must.NoError(t, err)
	must.Eq(t, "v", string(e.Value()))
	_, err = nodeJS.Stream(t.Context(), pushStreamName)
	must.Error(t, err)

	// A token needs a bus or cdc rule.
	policy := mock.ACLPolicy()
	policy.Name = "bus-events"
	policy.Rules = `bus { subscribe = ["app.>"] }`
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
	sub, err := consumer.SubscribeSync("app.events")
	must.NoError(t, err)
	must.NoError(t, consumer.Flush())
	must.NoError(t, mgmt.Publish("app.events", []byte("event")))
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

// TestBusServer_WorkloadIdentity checks that a task connects with its
// workload identity and gets the bus rules of the policies bound to its job,
// with its replies under _INBOX.<allocation ID>.<task>, and that the
// connection is closed when the allocation has ended. Before, the bus
// resolved only ACL token secrets: a policy bound to a job gave its tasks
// nothing on the bus.
func TestBusServer_WorkloadIdentity(t *testing.T) {
	ci.Parallel(t)

	s1, root, cleanup := TestACLServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	waitForActiveRootKey(t, s1)
	st := s1.fsm.State()

	// A running allocation of a job with a bound policy, and one of a job
	// without.
	running := func(index uint64) (*structs.Allocation, string) {
		alloc := mock.Alloc()
		alloc.ClientStatus = structs.AllocClientStatusRunning
		must.NoError(t, st.UpsertJob(structs.MsgTypeTestSetup, index, nil, alloc.Job))
		identity, _, err := s1.encrypter.SignClaims(alloc.ToTaskIdentityClaims(alloc.Job, "web"))
		must.NoError(t, err)
		must.NoError(t, st.UpsertAllocs(structs.MsgTypeTestSetup, index+1, []*structs.Allocation{alloc}))
		return alloc, identity
	}
	alloc, identity := running(100)
	_, unbound := running(110)
	policy := mock.ACLPolicy()
	policy.Rules = `bus {
  subscribe = ["svc.>"]
  publish   = ["svc.>"]
}`
	policy.JobACL = &structs.JobACL{Namespace: alloc.Namespace, JobID: alloc.JobID}
	policy.SetHash()
	must.NoError(t, st.UpsertACLPolicies(structs.MsgTypeTestSetup, 120, []*structs.ACLPolicy{policy}))

	srv, err := s1.bus.Connect()
	must.NoError(t, err)
	defer srv.Close()
	_, err = srv.Subscribe("svc.echo", func(m *nats.Msg) { _ = m.Respond([]byte("pong")) })
	must.NoError(t, err)
	must.NoError(t, srv.Flush())

	// The task's own inbox prefix: the reply arrives.
	closed := make(chan struct{})
	task, err := nats.Connect(busURL(s1), nats.Token(identity), nats.NoReconnect(),
		nats.CustomInboxPrefix(bus.InboxPrefix+"."+alloc.ID+".web"),
		nats.ClosedHandler(func(*nats.Conn) { close(closed) }))
	must.NoError(t, err)
	defer task.Close()
	reply, err := task.Request("svc.echo", []byte("ping"), 5*time.Second)
	must.NoError(t, err)
	must.Eq(t, "pong", string(reply.Data))

	// The inbox of a token is not the task's.
	other, err := nats.Connect(busURL(s1), nats.Token(identity),
		nats.CustomInboxPrefix(bus.InboxPrefix+"."+root.AccessorID))
	must.NoError(t, err)
	defer other.Close()
	_, err = other.Request("svc.echo", []byte("ping"), time.Second)
	must.Error(t, err)

	// A job without a bound bus rule, and a token that is not a valid
	// identity, are refused.
	_, err = nats.Connect(busURL(s1), nats.Token(unbound))
	must.Error(t, err)
	_, err = nats.Connect(busURL(s1), nats.Token(identity[:len(identity)-4]+"AAAA"))
	must.Error(t, err)

	// The status counts the workload connections.
	status, err := s1.busStatus()
	must.NoError(t, err)
	must.Eq(t, 2, status.Connections.Workloads)

	// The allocation ends: the connection is closed and the identity no
	// longer connects.
	// (The client reports it: a server-side upsert keeps the client status.)
	ended := alloc.Copy()
	ended.ClientStatus = structs.AllocClientStatusComplete
	must.NoError(t, st.UpdateAllocsFromClient(structs.MsgTypeTestSetup, 130, []*structs.Allocation{ended}))
	select {
	case <-closed:
	case <-time.After(3 * busRevalidateInterval):
		t.Fatal("the connection of the ended allocation was not closed")
	}
	_, err = nats.Connect(busURL(s1), nats.Token(identity))
	must.Error(t, err)
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
			CAFile:               dir + "norca-agent-ca.pem",
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
		cfg.ConsulKVMaxBytes = 16 << 20
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

	s1, addr1 := start("s1", "global-server-norca")
	s2, addr2 := start("s2", "global-server-norca")
	// A client certificate of the region, and a server certificate of
	// another CA, are not servers of this region.
	s3, addr3 := start("s3", "global-client-norca")
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
	ca, err := os.ReadFile(dir + "norca-agent-ca.pem")
	must.NoError(t, err)
	must.True(t, pool.AppendCertsFromPEM(ca))
	tlsConf := &tls.Config{RootCAs: pool, ServerName: "server.global.norca"}
	nc, err := nats.Connect(s1.ClientURL(), nats.Token("ok"), nats.Secure(tlsConf))
	must.NoError(t, err)
	nc.Close()
	_, err = nats.Connect(s1.ClientURL(), nats.Token("nope"), nats.Secure(tlsConf))
	must.Error(t, err)

	// The TLS of a clustered bus is reloaded in place; turning it off
	// needs a restart (review finding MUK-2).
	clientTLS, clusterTLS, err := busTLS(tlsFor("global-server-norca"), "global")
	must.NoError(t, err)
	must.NoError(t, s1.ReloadTLS(clientTLS, clusterTLS))
	must.ErrorIs(t, s1.ReloadTLS(nil, nil), bus.ErrTLSNeedsRestart)
	nc, err = nats.Connect(s1.ClientURL(), nats.Token("ok"), nats.Secure(tlsConf))
	must.NoError(t, err)
	nc.Close()
	st, err = s1.Status()
	must.NoError(t, err)
	must.True(t, st.TLS)
	must.SliceNotEmpty(t, st.Routes)
}

// A reloaded TLS configuration reaches the listener of the bus: a new CA
// and certificate are used by the connections made after the reload
// (review finding MUK-2).
func TestBusServer_ReloadTLS(t *testing.T) {
	ci.Parallel(t)

	const dir = "../helper/tlsutil/testdata/"
	s1, cleanup := TestServer(t, func(c *Config) {
		enableBus(t)(c)
		c.TLSConfig = &config.TLSConfig{
			EnableRPC:            true,
			VerifyServerHostname: true,
			CAFile:               dir + "norca-agent-ca.pem",
			CertFile:             dir + "global-server-norca.pem",
			KeyFile:              dir + "global-server-norca-key.pem",
		}
	})
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	node := mock.Node()
	must.NoError(t, s1.fsm.State().UpsertNode(structs.MsgTypeTestSetup, 100, node))

	// connect connects as the node over TLS, trusting the CA of caFile
	// only, with the client certificate of client when it is not nil.
	connect := func(caFile string, client *config.TLSConfig) error {
		pool := x509.NewCertPool()
		pem, err := os.ReadFile(caFile)
		must.NoError(t, err)
		must.True(t, pool.AppendCertsFromPEM(pem))
		conf := &tls.Config{RootCAs: pool, ServerName: "server.global.norca"}
		if client != nil {
			pair, err := tls.LoadX509KeyPair(client.CertFile, client.KeyFile)
			must.NoError(t, err)
			conf.Certificates = []tls.Certificate{pair}
		}
		nc, err := nats.Connect(busURL(s1), nats.UserInfo(node.ID, node.SecretID), nats.Secure(conf))
		if err == nil {
			nc.Close()
		}
		return err
	}
	must.NoError(t, connect(dir+"norca-agent-ca.pem", nil))

	// The CA and the certificate of the servers are replaced.
	pki := newBusPKI(t)
	nodeCA := pki.nodeCA("node CA")
	server := pki.agent("server", "server.global.norca", nodeCA, "")
	client := pki.agent("client", "client.global.norca", nodeCA, "")
	must.NoError(t, s1.reloadTLSConnections(server))

	must.NoError(t, connect(server.CAFile, nil), must.Sprint("the bus kept the certificate of before the reload"))
	// A client certificate of the new CA is verified with the new CA.
	must.NoError(t, connect(server.CAFile, client), must.Sprint("the bus kept the CA of before the reload"))
	must.Error(t, connect(dir+"norca-agent-ca.pem", nil))
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"
)

// startLeafNode starts a NATS server with a JetStream of its own, as the
// nats pack runs, attached to the bus of s as a leaf node with the secret
// of a token.
func startLeafNode(t *testing.T, s *Server, secret string) *natsserver.Server {
	t.Helper()
	store, err := os.MkdirTemp("", "norca-leaf-test")
	must.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(store) })
	u, err := url.Parse("nats-leaf://" + secret + "@127.0.0.1:" + strconv.Itoa(s.config.Bus.LeafNodes.Port))
	must.NoError(t, err)
	ns, err := natsserver.NewServer(&natsserver.Options{
		ServerName: fmt.Sprintf("leaf-%d", time.Now().UnixNano()),
		Host:       "127.0.0.1",
		Port:       -1,
		JetStream:  true,
		StoreDir:   store,
		NoSigs:     true,
		LeafNode: natsserver.LeafNodeOpts{
			ReconnectInterval: 200 * time.Millisecond,
			Remotes:           []*natsserver.RemoteLeafOpts{{URLs: []*url.URL{u}}},
		},
	})
	must.NoError(t, err)
	go ns.Start()
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	must.True(t, ns.ReadyForConnections(10*time.Second))
	return ns
}

func TestCDCLeaf_Streams(t *testing.T) {
	ci.Parallel(t)
	s1, _, cleanup := TestACLServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	st := s1.fsm.State()

	policy := func(name, rules string) *structs.ACLPolicy {
		p := mock.ACLPolicy()
		p.Name, p.Rules = name, rules
		p.SetHash()
		return p
	}
	policies := []*structs.ACLPolicy{
		policy("all", `cdc { receive = ["norca.>"] leaf = true }`),
		policy("two", `cdc { receive = ["norca.global.default.Job", "norca.global.default.Allocation"] leaf = true }`),
		policy("overlap", `cdc { receive = ["norca.*.default.>", "norca.global.*.Job"] leaf = true }`),
		policy("elsewhere", `cdc { receive = ["norca.us.>"] leaf = true }`),
		policy("nodes", `cdc { receive = ["norca.>"] leaf = true }
node { policy = "read" }`),
	}
	must.NoError(t, st.UpsertACLPolicies(structs.MsgTypeTestSetup, 100, policies))

	streams := func(name string) ([]cdcLeafStream, error) {
		tok := mock.ACLToken()
		tok.Policies = []string{name}
		aclObj, err := s1.ResolveACLForToken(tok)
		must.NoError(t, err)
		return s1.cdcLeafStreams(aclObj, aclObj.CDCGrant(), true)
	}
	events, kv, admin := bus.EventStreamName("global"), bus.ConsulKVChangesStreamName("global"), bus.AdminEventStreamName("global")

	got, err := streams("all")
	must.NoError(t, err)
	must.Eq(t, []cdcLeafStream{
		{name: events, subjects: []string{"norca.global.*.*"}, consumer: true},
		{name: kv, subjects: []string{"norca.global.*.ConsulKV.*"}, consumer: true},
	}, got)

	got, err = streams("two")
	must.NoError(t, err)
	must.Eq(t, []cdcLeafStream{
		{name: events, subjects: []string{"norca.global.default.Allocation", "norca.global.default.Job"}, consumer: true},
	}, got)

	// The admin topics the token may see, live and without a consumer.
	got, err = streams("nodes")
	must.NoError(t, err)
	must.Len(t, 3, got)
	must.Eq(t, cdcLeafStream{name: admin, subjects: []string{"norca.admin.global.*.Node"}}, got[2])

	// A management token: every change stream, with a consumer of the admin
	// events too.
	got, err = s1.cdcLeafStreams(acl.ManagementACL, acl.ManagementACL.CDCGrant(), true)
	must.NoError(t, err)
	must.Eq(t, []cdcLeafStream{
		{name: events, subjects: []string{"norca.global.*.*"}, consumer: true},
		{name: kv, subjects: []string{"norca.global.*.ConsulKV.*"}, consumer: true},
		{name: admin, subjects: []string{"norca.admin.global.>"}, consumer: true},
	}, got)

	_, err = streams("overlap")
	must.ErrorContains(t, err, "overlap")
	_, err = streams("elsewhere")
	must.ErrorContains(t, err, "no change stream")
}

// A NATS server attaches to the bus as a leaf node with a token whose cdc
// rule allows it: it receives the changes of its rule live and reads the
// consumer the servers make for it; a token without the leaf right attaches
// nothing; deleting the token closes the leaf and removes its consumers.
func TestCDCLeaf_Attach(t *testing.T) {
	ci.Parallel(t)
	s1, root, cleanup := TestACLServer(t, func(c *Config) {
		enableBus(t)(c)
		c.Bus.LeafNodes = bus.LeafNodesConfig{Enabled: true, Port: ci.PortAllocator.Grab(1)[0]}
	})
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	st := s1.fsm.State()

	leafPolicy := mock.ACLPolicy()
	leafPolicy.Name = "leaf"
	leafPolicy.Rules = `cdc {
  receive = ["norca.global.default.>"]
  leaf    = true
}`
	leafPolicy.SetHash()
	readerPolicy := mock.ACLPolicy()
	readerPolicy.Name = "reader"
	readerPolicy.Rules = `cdc { receive = ["norca.>"] }`
	readerPolicy.SetHash()
	must.NoError(t, st.UpsertACLPolicies(structs.MsgTypeTestSetup, 100, []*structs.ACLPolicy{leafPolicy, readerPolicy}))
	leafTok, readerTok := mock.ACLToken(), mock.ACLToken()
	leafTok.Policies = []string{leafPolicy.Name}
	leafTok.SetHash()
	readerTok.Policies = []string{readerPolicy.Name}
	readerTok.SetHash()
	must.NoError(t, st.UpsertACLTokens(structs.MsgTypeTestSetup, 101, []*structs.ACLToken{leafTok, readerTok}))

	js := busJS(t, s1)
	for _, name := range []string{bus.EventStreamName("global"), bus.ConsulKVChangesStreamName("global")} {
		waitBus(t, func() (bool, error) {
			_, err := js.Stream(t.Context(), name)
			return err == nil, err
		})
	}

	leaf := startLeafNode(t, s1, leafTok.SecretID)
	waitBus(t, func() (bool, error) {
		return leaf.NumLeafNodes() == 1, errors.New("the leaf node did not attach")
	})
	status, err := s1.busStatus()
	must.NoError(t, err)
	must.Eq(t, 1, status.Connections.Leaves)

	// The servers made its consumers, with the subjects of its rule inside
	// each stream.
	consumer := "leaf-" + leafTok.AccessorID
	for stream, filter := range map[string]string{
		bus.EventStreamName("global"):           "norca.global.default.*",
		bus.ConsulKVChangesStreamName("global"): "norca.global.default.ConsulKV.*",
	} {
		waitBus(t, func() (bool, error) {
			c, err := js.Consumer(t.Context(), stream, consumer)
			if err != nil {
				return false, err
			}
			info := c.CachedInfo()
			if len(info.Config.FilterSubjects) != 1 || info.Config.FilterSubjects[0] != filter {
				return false, fmt.Errorf("filters %v", info.Config.FilterSubjects)
			}
			must.Eq(t, "leaf:"+leafTok.AccessorID, info.Config.Metadata[cdcReaderMeta])
			return true, nil
		})
	}

	// A client of the leaf's NATS receives a change live.
	app, err := nats.Connect(leaf.ClientURL())
	must.NoError(t, err)
	defer app.Close()
	live, err := app.SubscribeSync("norca.>")
	must.NoError(t, err)
	must.NoError(t, app.Flush())
	time.Sleep(500 * time.Millisecond)

	job := mock.Job()
	codec := rpcClient(t, s1)
	var regResp structs.JobRegisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", &structs.JobRegisterRequest{
		Job:          job,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace, AuthToken: root.SecretID},
	}, &regResp))
	msg, err := live.NextMsg(30 * time.Second)
	must.NoError(t, err)
	must.StrHasPrefix(t, "norca.global.default.", msg.Subject)

	// And reads its consumer through the domain of the region.
	ljs, err := jetstream.NewWithDomain(app, "norca-global")
	must.NoError(t, err)
	cons, err := ljs.Consumer(t.Context(), bus.EventStreamName("global"), consumer)
	must.NoError(t, err)
	batch, err := cons.Fetch(10, jetstream.FetchMaxWait(2*time.Second))
	must.NoError(t, err)
	n := 0
	for m := range batch.Messages() {
		must.StrHasPrefix(t, "norca.global.default.", m.Subject())
		must.NoError(t, m.Ack())
		n++
	}
	must.NoError(t, batch.Error())
	must.Positive(t, n)

	// A token without the leaf right attaches no leaf.
	other := startLeafNode(t, s1, readerTok.SecretID)
	time.Sleep(time.Second)
	must.Eq(t, 0, other.NumLeafNodes())
	other.Shutdown()

	// Deleting the token closes the leaf and removes its consumers.
	must.NoError(t, st.DeleteACLTokens(structs.MsgTypeTestSetup, 102, []string{leafTok.AccessorID}))
	waitBus(t, func() (bool, error) {
		return leaf.NumLeafNodes() == 0, errors.New("the leaf node is still attached")
	})
	waitBus(t, func() (bool, error) {
		_, err := js.Consumer(t.Context(), bus.EventStreamName("global"), consumer)
		if errors.Is(err, jetstream.ErrConsumerNotFound) {
			return true, nil
		}
		return false, fmt.Errorf("the consumer of the leaf node is still there: %v", err)
	})
}

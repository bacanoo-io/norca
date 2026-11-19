// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package bus

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/testutil"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"
)

// The connections of the bus and its streams are in the account NORCA.
func TestBus_Account(t *testing.T) {
	ci.Parallel(t)
	authz := newTestAuthz()
	c := testConfig(t, "s1")
	s := startServer(t, c, authz.authorize)
	testutil.WaitForResult(func() (bool, error) {
		return s.JetStreamReady(), errors.New("no JetStream meta leader")
	}, func(err error) { t.Fatal(err) })

	nc, err := s.Connect()
	must.NoError(t, err)
	defer nc.Close()
	js, err := jetstream.New(nc)
	must.NoError(t, err)
	createStream(t, js, jetstream.StreamConfig{Name: "TEST", Subjects: []string{"test.>"}})

	reader, err := nats.Connect(s.ClientURL(), nats.Token("reader"))
	must.NoError(t, err)
	defer reader.Close()
	node, err := nats.Connect(s.ClientURL(), nats.UserInfo("n1", "s1"))
	must.NoError(t, err)
	defer node.Close()

	connz, err := s.server().Connz(&natsserver.ConnzOptions{Username: true})
	must.NoError(t, err)
	must.Len(t, 3, connz.Conns)
	for _, ci := range connz.Conns {
		must.Eq(t, AccountName, ci.Account, must.Sprintf("connection %s", ci.AuthorizedUser))
	}

	jsz, err := s.server().Jsz(&natsserver.JSzOptions{Accounts: true, Streams: true})
	must.NoError(t, err)
	must.Len(t, 1, jsz.AccountDetails)
	must.Eq(t, AccountName, jsz.AccountDetails[0].Name)
	must.Len(t, 1, jsz.AccountDetails[0].Streams)
	_, err = os.Stat(filepath.Join(c.StoreDir, "jetstream", AccountName, "streams", "TEST"))
	must.NoError(t, err)
}

func TestReaderAccountName(t *testing.T) {
	ci.Parallel(t)
	a := &ReaderSpec{Reader: "r1", Subjects: []string{"norca.eu.*.Job"}}
	b := &ReaderSpec{Reader: "r1", Subjects: []string{"norca.eu.*.Job", "norca.eu.*.Allocation"}}
	must.StrHasPrefix(t, ReaderAccountPrefix+"r1_", ReaderAccountName(a))
	must.Eq(t, ReaderAccountName(a), ReaderAccountName(&ReaderSpec{Reader: "r1", Subjects: []string{"norca.eu.*.Job"}}))
	must.NotEq(t, ReaderAccountName(a), ReaderAccountName(b))
	must.NotEq(t, ReaderAccountName(a), ReaderAccountName(&ReaderSpec{Reader: "r2", Subjects: a.Subjects}))

	// The information subjects are imports too: another account.
	info := &ReaderSpec{Reader: "r1", Subjects: a.Subjects, Info: true,
		Consumers: []ConsumerRef{{Stream: "S1", Name: "r1"}, {Stream: "S2", Name: "r1"}, {Stream: "S1", Name: "r1"}}}
	plain := &ReaderSpec{Reader: "r1", Subjects: a.Subjects, Consumers: info.Consumers}
	must.NotEq(t, ReaderAccountName(plain), ReaderAccountName(info))
	must.Eq(t, []string{"$JS.API.INFO", "$JS.API.STREAM.INFO.S1", "$JS.API.STREAM.INFO.S2"}, info.InfoSubjects())
	must.SliceEmpty(t, plain.InfoSubjects())
}

func TestBus_ReaderAccountRefused(t *testing.T) {
	ci.Parallel(t)
	s := startServer(t, testConfig(t, "s1"), newTestAuthz().authorize)
	events := ConsumerRef{Stream: EventStreamName("global"), Name: "r1"}

	cases := []struct {
		name string
		spec *ReaderSpec
	}{
		{"no reader", &ReaderSpec{Subjects: []string{"norca.global.*.Job"}}},
		{"reader with a dot", &ReaderSpec{Reader: "a.b", Subjects: []string{"norca.global.*.Job"}}},
		{"nothing imported", &ReaderSpec{Reader: "r1"}},
		{"telemetry", &ReaderSpec{Reader: "r1", Subjects: []string{"norca.telemetry.global.n1"}}},
		{"pack push notices", &ReaderSpec{Reader: "r1", Subjects: []string{"norca.packs.pushed"}}},
		{"consul kv values", &ReaderSpec{Reader: "r1", Subjects: []string{"$KV.consul-kv.>"}}},
		{"jetstream api", &ReaderSpec{Reader: "r1", Subjects: []string{"$JS.API.>"}}},
		{"wider than the stream", &ReaderSpec{Reader: "r1", Subjects: []string{"norca.global.>"}}},
		{"another region", &ReaderSpec{Reader: "r1", Subjects: []string{"norca.eu.*.Job"}}},
		{"invalid subject", &ReaderSpec{Reader: "r1", Subjects: []string{"norca..x"}}},
		{"telemetry stream", &ReaderSpec{Reader: "r1", Consumers: []ConsumerRef{{Stream: TelemetryStreamName("global"), Name: "r1"}}}},
		{"consumer wildcard", &ReaderSpec{Reader: "r1", Consumers: []ConsumerRef{{Stream: events.Stream, Name: "*"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.ReaderAccount(tc.spec)
			must.Error(t, err)
		})
	}

	// The subjects of the three change streams are accepted.
	_, err := s.ReaderAccount(&ReaderSpec{Reader: "r1",
		Subjects: []string{
			"norca.global.*.*",
			"norca.global.dev.ConsulKV.*",
			"norca.admin.global.*.Node",
		},
		Consumers: []ConsumerRef{events,
			{Stream: AdminEventStreamName("global"), Name: "r1"},
			{Stream: ConsulKVChangesStreamName("global"), Name: "r1"},
		},
	})
	must.NoError(t, err)
}

// A reader account receives what the bus account exports to it, and
// nothing else; nothing it publishes reaches the bus account.
func TestBus_ReaderAccount(t *testing.T) {
	ci.Parallel(t)
	t.Run("single server", func(t *testing.T) {
		ci.Parallel(t)
		authz := newTestAuthz()
		s := startServer(t, testConfig(t, "s1"), authz.authorize)
		testutil.WaitForResult(func() (bool, error) {
			return s.JetStreamReady(), errors.New("no JetStream meta leader")
		}, func(err error) { t.Fatal(err) })
		testReaderAccount(t, authz, s, s, 1)
	})
	t.Run("cluster", func(t *testing.T) {
		ci.Parallel(t)
		authz := newTestAuthz()
		var servers []*Server
		var addrs []string
		for i := range 2 {
			c := testConfig(t, fmt.Sprintf("s%d", i+1))
			c.Clustered = true
			addrs = append(addrs, net.JoinHostPort(c.BindAddr, strconv.Itoa(c.ClusterPort)))
			servers = append(servers, startServer(t, c, authz.authorize))
		}
		must.NoError(t, servers[0].SetRoutes(addrs[1:]))
		must.NoError(t, servers[1].SetRoutes(addrs[:1]))
		testutil.WaitForResult(func() (bool, error) {
			for _, s := range servers {
				st, err := s.Status()
				if err != nil {
					return false, err
				}
				if st.JetStream == nil || st.JetStream.MetaLeader == "" || st.JetStream.ClusterSize != 2 {
					return false, fmt.Errorf("%s: JetStream %+v", st.Server, st.JetStream)
				}
			}
			return true, nil
		}, func(err error) { t.Fatal(err) })
		// The stream is written through one server, the reader account is on
		// the other.
		testReaderAccount(t, authz, servers[0], servers[1], 2)
	})
}

func testReaderAccount(t *testing.T, authz *testAuthz, writer, readerSrv *Server, replicas int) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	stream := EventStreamName("global")

	nc, err := writer.Connect()
	must.NoError(t, err)
	defer nc.Close()
	js, err := jetstream.New(nc)
	must.NoError(t, err)
	createStream(t, js, jetstream.StreamConfig{Name: stream, Subjects: EventStreamSubjects("norca", "global"), Replicas: replicas})
	newConsumer := func(name, filter string) {
		testutil.WaitForResult(func() (bool, error) {
			_, err := js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{
				Durable: name, FilterSubject: filter, AckPolicy: jetstream.AckExplicitPolicy, Replicas: replicas,
			})
			return err == nil, err
		}, func(err error) { t.Fatal(err) })
	}
	newConsumer("r1", "norca.global.*.Job")
	newConsumer("other", "norca.global.*.Node")

	spec := &ReaderSpec{
		Reader:    "r1",
		Subjects:  []string{"norca.global.*.Job"},
		Consumers: []ConsumerRef{{Stream: stream, Name: "r1"}},
	}
	name, err := readerSrv.ReaderAccount(spec)
	must.NoError(t, err)
	again, err := readerSrv.ReaderAccount(spec)
	must.NoError(t, err)
	must.Eq(t, name, again)

	// The reader's connection may do anything in its account.
	authz.set("cdc", &Grant{Kind: KindToken, Name: "r1", All: true, Account: name})
	reader, err := nats.Connect(readerSrv.ClientURL(), nats.Token("cdc"))
	must.NoError(t, err)
	defer reader.Close()

	// The bus account sees nothing the reader publishes.
	leaked, err := nc.SubscribeSync(">")
	must.NoError(t, err)
	must.NoError(t, nc.Flush())

	live, err := reader.SubscribeSync(">")
	must.NoError(t, err)
	must.NoError(t, reader.Flush())
	// The interest of the reader reaches the writer's server.
	time.Sleep(200 * time.Millisecond)

	for _, subj := range []string{"norca.global.default.Node", "norca.global.default.Job"} {
		_, err := js.Publish(ctx, subj, []byte(subj))
		must.NoError(t, err)
	}
	must.NoError(t, nc.Publish("_INBOX.someone.1", []byte("reply")))
	must.NoError(t, nc.Publish("norca.telemetry.global.n1", []byte("usage")))
	must.NoError(t, nc.Flush())

	msg, err := live.NextMsg(5 * time.Second)
	must.NoError(t, err)
	must.Eq(t, "norca.global.default.Job", msg.Subject)
	_, err = live.NextMsg(300 * time.Millisecond)
	must.ErrorIs(t, err, nats.ErrTimeout)

	// Its consumer: information, fetch and acknowledgement.
	rjs, err := jetstream.New(reader)
	must.NoError(t, err)
	var cons jetstream.Consumer
	testutil.WaitForResult(func() (bool, error) {
		c, err := rjs.Consumer(ctx, stream, "r1")
		cons = c
		return err == nil, err
	}, func(err error) { t.Fatal(err) })
	batch, err := cons.Fetch(10, jetstream.FetchMaxWait(2*time.Second))
	must.NoError(t, err)
	var got []string
	for m := range batch.Messages() {
		got = append(got, m.Subject())
		must.NoError(t, m.Ack())
	}
	must.NoError(t, batch.Error())
	must.Eq(t, []string{"norca.global.default.Job"}, got)
	must.NoError(t, reader.Flush())
	testutil.WaitForResult(func() (bool, error) {
		bc, err := js.Consumer(ctx, stream, "r1")
		if err != nil {
			return false, err
		}
		info := bc.CachedInfo()
		if info.AckFloor.Stream == 0 || info.NumAckPending != 0 {
			return false, fmt.Errorf("ack floor %d, pending %d", info.AckFloor.Stream, info.NumAckPending)
		}
		return true, nil
	}, func(err error) { t.Fatal(err) })

	// Nothing else of the JetStream API: no other consumer, no stream.
	short, cancelShort := context.WithTimeout(ctx, time.Second)
	defer cancelShort()
	_, err = rjs.Consumer(short, stream, "other")
	must.Error(t, err)
	_, err = rjs.Stream(short, stream)
	must.Error(t, err)
	_, err = rjs.CreateOrUpdateConsumer(short, stream, jetstream.ConsumerConfig{Durable: "mine"})
	must.Error(t, err)
	_, err = rjs.KeyValue(short, "consul-kv")
	must.Error(t, err)

	// What the reader publishes, also on an exported subject, stays in its
	// account.
	must.NoError(t, reader.Publish("norca.global.default.Job", []byte("forged")))
	must.NoError(t, reader.Publish("$JS.API.STREAM.DELETE."+stream, nil))
	must.NoError(t, reader.Flush())
	for {
		m, err := leaked.NextMsg(500 * time.Millisecond)
		if errors.Is(err, nats.ErrTimeout) {
			break
		}
		must.NoError(t, err)
		must.NotEq(t, "forged", string(m.Data), must.Sprintf("subject %s", m.Subject))
		must.NotEq(t, "$JS.API.STREAM.DELETE."+stream, m.Subject)
	}
	_, err = js.Stream(ctx, stream)
	must.NoError(t, err)

	// A grant naming an account that does not exist is refused.
	authz.set("ghost", &Grant{Kind: KindToken, Name: "ghost", All: true, Account: ReaderAccountPrefix + "missing"})
	_, err = nats.Connect(readerSrv.ClientURL(), nats.Token("ghost"), nats.MaxReconnects(0))
	must.Error(t, err)
}

// A route change reloads the options of the embedded server. nats-server
// then closes the connections the custom authentication registered and
// deletes the reader accounts made at runtime: the connections reconnect,
// and a change data capture reader gets its account again and receives
// what the bus account exports to it (review finding MUK-22).
func TestBus_RouteChangeKeepsReaders(t *testing.T) {
	ci.Parallel(t)
	authz := newTestAuthz()
	c := testConfig(t, "s1")
	c.Clustered = true
	s := startServer(t, c, authz.authorize)

	// The reader's account is made when its connection is authenticated,
	// as the Norca server does.
	spec := &ReaderSpec{Reader: "r1", Subjects: []string{"norca.global.*.Job"}}
	name := ReaderAccountName(spec)
	authz.withReader(func() (*Grant, error) {
		if _, err := s.ReaderAccount(spec); err != nil {
			return nil, err
		}
		return &Grant{Kind: KindToken, Name: "r1", All: true, Account: name}, nil
	})

	connect := func(opts ...nats.Option) *nats.Conn {
		nc, err := nats.Connect(s.ClientURL(), append(opts, nats.MaxReconnects(-1), nats.ReconnectWait(20*time.Millisecond))...)
		must.NoError(t, err)
		t.Cleanup(nc.Close)
		return nc
	}
	conns := map[string]*nats.Conn{
		"node":   connect(nats.UserInfo("n1", "s1")),
		"token":  connect(nats.Token("admin")),
		"reader": connect(nats.Token("cdc")),
	}
	internal, err := s.Connect()
	must.NoError(t, err)
	defer internal.Close()
	conns["internal"] = internal
	live, err := conns["reader"].SubscribeSync(">")
	must.NoError(t, err)
	must.NoError(t, conns["reader"].Flush())

	must.NoError(t, s.SetRoutes([]string{"127.0.0.1:1"}))

	testutil.WaitForResult(func() (bool, error) {
		for what, nc := range conns {
			if !nc.IsConnected() {
				return false, fmt.Errorf("%s: %v (%v)", what, nc.Status(), nc.LastError())
			}
		}
		return true, nil
	}, func(err error) { t.Fatal(err) })
	_, err = s.server().LookupAccount(name)
	must.NoError(t, err, must.Sprint("the reader account was not made again"))
	// The subscription of the reader is made again with its connection.
	testutil.WaitForResult(func() (bool, error) {
		if err := internal.Publish("norca.global.default.Job", []byte("job")); err != nil {
			return false, err
		}
		if err := internal.Flush(); err != nil {
			return false, err
		}
		msg, err := live.NextMsg(200 * time.Millisecond)
		if err != nil {
			return false, fmt.Errorf("the reader receives nothing: %w", err)
		}
		return msg.Subject == "norca.global.default.Job", fmt.Errorf("subject %s", msg.Subject)
	}, func(err error) { t.Fatal(err) })
}

// leafReads reports why a client of the leaf's NATS does not receive a Job
// event live or can't fetch one from the consumer leaf-leafer; nil when it
// does both.
func leafReads(ctx context.Context, js jetstream.JetStream, leafURL, stream string) error {
	app, err := nats.Connect(leafURL)
	if err != nil {
		return err
	}
	defer app.Close()
	live, err := app.SubscribeSync("norca.>")
	if err != nil {
		return err
	}
	if err := app.Flush(); err != nil {
		return err
	}
	// The interest of the leaf reaches the bus after a moment.
	for deadline := time.Now().Add(3 * time.Second); ; {
		if _, err := js.Publish(ctx, "norca.global.default.Job", []byte("job")); err != nil {
			return err
		}
		if _, err := live.NextMsg(300 * time.Millisecond); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("no Job event live")
		}
	}
	ljs, err := jetstream.NewWithDomain(app, "norca-global")
	if err != nil {
		return err
	}
	short, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cons, err := ljs.Consumer(short, stream, "leaf-leafer")
	if err != nil {
		return fmt.Errorf("the consumer: %w", err)
	}
	batch, err := cons.Fetch(1, jetstream.FetchMaxWait(2*time.Second))
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	n := 0
	for m := range batch.Messages() {
		if err := m.Ack(); err != nil {
			return err
		}
		n++
	}
	if err := batch.Error(); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("fetched %d messages", n)
	}
	return nil
}

// A route change reloads the options of the embedded server, which deletes
// the reader accounts. nats-server closes the client connections, but keeps
// a leaf node in its deleted account, where its consumer can't be reached
// any more. The bus closes the leaf: it attaches again into a new account and
// reads its consumer (review finding CON-5: the compaction of the reader
// accounts reloads the same way).
func TestBus_RouteChangeKeepsLeafReaders(t *testing.T) {
	ci.Parallel(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	var servers []*Server
	var authzs []*leafAuthz
	var addrs []string
	var configs []*Config
	for i := range 2 {
		c := testConfig(t, fmt.Sprintf("s%d", i+1))
		c.Clustered = true
		c.LeafNodes = LeafNodesConfig{Enabled: true, Port: ci.PortAllocator.Grab(1)[0]}
		addrs = append(addrs, net.JoinHostPort(c.BindAddr, strconv.Itoa(c.ClusterPort)))
		a := &leafAuthz{}
		s := startServer(t, c, a.authorize)
		a.set(s, nil)
		servers, authzs, configs = append(servers, s), append(authzs, a), append(configs, c)
	}
	must.NoError(t, servers[0].SetRoutes(addrs[1:]))
	must.NoError(t, servers[1].SetRoutes(addrs[:1]))
	testutil.WaitForResult(func() (bool, error) {
		for _, s := range servers {
			st, err := s.Status()
			if err != nil {
				return false, err
			}
			if st.JetStream == nil || st.JetStream.MetaLeader == "" || st.JetStream.ClusterSize != 2 {
				return false, fmt.Errorf("%s: JetStream %+v", st.Server, st.JetStream)
			}
		}
		return true, nil
	}, func(err error) { t.Fatal(err) })

	stream := EventStreamName("global")
	nc, err := servers[1].Connect()
	must.NoError(t, err)
	defer nc.Close()
	js, err := jetstream.New(nc)
	must.NoError(t, err)
	createStream(t, js, jetstream.StreamConfig{Name: stream, Subjects: EventStreamSubjects("norca", "global"), Replicas: 2})
	testutil.WaitForResult(func() (bool, error) {
		_, err := js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{
			Durable: "leaf-leafer", FilterSubject: "norca.global.*.Job", AckPolicy: jetstream.AckExplicitPolicy, Replicas: 2,
		})
		return err == nil, err
	}, func(err error) { t.Fatal(err) })
	for _, a := range authzs {
		a.set(nil, leafSpec(stream))
	}

	leaf := startLeaf(t, configs[0].LeafNodes.Port, "leafer")
	waitLeaf(t, leaf, 1)
	testutil.WaitForResultUntil(30*time.Second, func() (bool, error) {
		err := leafReads(ctx, js, leaf.ClientURL(), stream)
		return err == nil, err
	}, func(err error) { t.Fatal(err) })

	// A route to a server that is not there yet.
	must.NoError(t, servers[0].SetRoutes(append([]string{"127.0.0.1:1"}, addrs[1:]...)))
	testutil.WaitForResultUntil(30*time.Second, func() (bool, error) {
		err := leafReads(ctx, js, leaf.ClientURL(), stream)
		return err == nil, err
	}, func(err error) { t.Fatalf("the leaf does not read after the route change: %v", err) })
}

// busImports counts the imports of an account from the bus account.
func busImports(t *testing.T, s *Server, account string) int {
	t.Helper()
	az, err := s.server().Accountz(&natsserver.AccountzOptions{Account: account})
	must.NoError(t, err)
	must.NotNil(t, az.Account)
	n := 0
	for _, imp := range az.Account.Imports {
		if imp.Account == AccountName {
			n++
		}
	}
	return n
}

// readerTestSpec is the spec of a reader that connects directly: the Job
// events live, its consumer of the event stream and the information.
func readerTestSpec(reader string) *ReaderSpec {
	return &ReaderSpec{
		Reader:    reader,
		Subjects:  []string{"norca.global.*.Job"},
		Consumers: []ConsumerRef{{Stream: EventStreamName("global"), Name: reader}},
		Info:      true,
	}
}

// A reader account that had no connection for the grace period is retired:
// it imports nothing and takes no connection. A reader that comes back gets
// a new account that works, and a reader with a connection keeps its own
// (review finding CON-5).
func TestBus_ReaderAccountRetire(t *testing.T) {
	ci.Parallel(t)
	authz := newTestAuthz()
	s := startServer(t, testConfig(t, "s1"), authz.authorize)
	internal, err := s.Connect()
	must.NoError(t, err)
	defer internal.Close()

	// connect logs a reader in with the grant of its account.
	connect := func(reader, account string, opts ...nats.Option) (*nats.Conn, error) {
		authz.set(reader, &Grant{Kind: KindToken, Name: reader, All: true, Account: account})
		return nats.Connect(s.ClientURL(), append(opts, nats.Token(reader))...)
	}
	// receives waits until sub gets a Job event.
	receives := func(what string, sub *nats.Subscription) {
		t.Helper()
		testutil.WaitForResult(func() (bool, error) {
			if err := internal.Publish("norca.global.default.Job", []byte("job")); err != nil {
				return false, err
			}
			if err := internal.Flush(); err != nil {
				return false, err
			}
			_, err := sub.NextMsg(200 * time.Millisecond)
			return err == nil, err
		}, func(err error) { t.Fatalf("%s receives nothing: %v", what, err) })
	}

	live, gone := readerTestSpec("live"), readerTestSpec("gone")
	liveName, err := s.ReaderAccount(live)
	must.NoError(t, err)
	goneName, err := s.ReaderAccount(gone)
	must.NoError(t, err)
	lc, err := connect("live", liveName)
	must.NoError(t, err)
	defer lc.Close()
	liveSub, err := lc.SubscribeSync(">")
	must.NoError(t, err)
	must.NoError(t, lc.Flush())
	receives("the live reader", liveSub)

	// The other reader connects and leaves.
	gc, err := connect("gone", goneName)
	must.NoError(t, err)
	gc.Close()
	goneAcc, err := s.server().LookupAccount(goneName)
	must.NoError(t, err)
	imports := busImports(t, s, goneName)
	must.Positive(t, imports)
	testutil.WaitForResult(func() (bool, error) {
		n := goneAcc.NumConnections()
		return n == 0, fmt.Errorf("%d connections", n)
	}, func(err error) { t.Fatal(err) })

	s.sweepReaders(time.Now().Add(readerRetireGrace))

	// Retired: it imports nothing from the bus account and takes no
	// connection.
	must.Eq(t, 0, busImports(t, s, goneName), must.Sprint("the account of the reader that left still imports"))
	_, err = connect("gone", goneName, nats.MaxReconnects(0))
	must.Error(t, err)

	// The live reader keeps its account.
	again, err := s.ReaderAccount(live)
	must.NoError(t, err)
	must.Eq(t, liveName, again)
	receives("the live reader", liveSub)

	// The reader that comes back gets a new account that works.
	back, err := s.ReaderAccount(gone)
	must.NoError(t, err)
	must.NotEq(t, goneName, back)
	must.Eq(t, imports, busImports(t, s, back))
	bc, err := connect("gone", back)
	must.NoError(t, err)
	defer bc.Close()
	backSub, err := bc.SubscribeSync(">")
	must.NoError(t, err)
	must.NoError(t, bc.Flush())
	receives("the reader that came back", backSub)
}

// Once the retired reader accounts are due, they are dropped by a reload of
// the options: the accounts are gone, and so are the exports of the bus
// account to them. The live readers, a client and a leaf node, reconnect into
// new accounts and read again (review finding CON-5).
func TestBus_ReaderAccountCompaction(t *testing.T) {
	ci.Parallel(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	leafs, clients := &leafAuthz{}, newTestAuthz()
	c := testConfig(t, "s1")
	c.LeafNodes = LeafNodesConfig{Enabled: true, Port: ci.PortAllocator.Grab(1)[0]}
	// The configured threshold decides: four retired accounts are dropped
	// with 3, and would not be with the default.
	c.ReaderCompactThreshold = 3
	s := startServer(t, c, func(cr Credentials) (*Grant, error) {
		if cr.Leaf {
			return leafs.authorize(cr)
		}
		return clients.authorize(cr)
	})
	s.setReaderRetirement(time.Minute, time.Hour)
	testutil.WaitForResult(func() (bool, error) {
		return s.JetStreamReady(), errors.New("no JetStream meta leader")
	}, func(err error) { t.Fatal(err) })

	stream := EventStreamName("global")
	nc, err := s.Connect()
	must.NoError(t, err)
	defer nc.Close()
	js, err := jetstream.New(nc)
	must.NoError(t, err)
	createStream(t, js, jetstream.StreamConfig{Name: stream, Subjects: EventStreamSubjects("norca", "global")})
	for _, name := range []string{"leaf-leafer", "live"} {
		_, err := js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{
			Durable: name, FilterSubject: "norca.global.*.Job", AckPolicy: jetstream.AckExplicitPolicy,
		})
		must.NoError(t, err)
	}
	leafs.set(s, leafSpec(stream))
	live := readerTestSpec("live")
	clients.withReader(func() (*Grant, error) {
		name, err := s.ReaderAccount(live)
		if err != nil {
			return nil, err
		}
		return &Grant{Kind: KindToken, Name: "live", All: true, Account: name}, nil
	})

	reader, err := nats.Connect(s.ClientURL(), nats.Token("cdc"), nats.MaxReconnects(-1),
		nats.ReconnectWait(20*time.Millisecond))
	must.NoError(t, err)
	defer reader.Close()
	liveSub, err := reader.SubscribeSync("norca.>")
	must.NoError(t, err)
	must.NoError(t, reader.Flush())
	// readerReads reports why the client reader does not receive a Job event
	// live or can't fetch one from its consumer.
	readerReads := func() error {
		if _, err := js.Publish(ctx, "norca.global.default.Job", []byte("job")); err != nil {
			return err
		}
		if _, err := liveSub.NextMsg(300 * time.Millisecond); err != nil {
			return fmt.Errorf("live: %w", err)
		}
		rjs, err := jetstream.New(reader)
		if err != nil {
			return err
		}
		short, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		cons, err := rjs.Consumer(short, stream, "live")
		if err != nil {
			return fmt.Errorf("the consumer: %w", err)
		}
		batch, err := cons.Fetch(1, jetstream.FetchMaxWait(2*time.Second))
		if err != nil {
			return fmt.Errorf("fetch: %w", err)
		}
		for m := range batch.Messages() {
			if err := m.Ack(); err != nil {
				return err
			}
		}
		return batch.Error()
	}
	leaf := startLeaf(t, c.LeafNodes.Port, "leafer")
	waitLeaf(t, leaf, 1)
	reads := func(what string) {
		t.Helper()
		testutil.WaitForResultUntil(30*time.Second, func() (bool, error) {
			err := readerReads()
			return err == nil, err
		}, func(err error) { t.Fatalf("the client reader %s: %v", what, err) })
		testutil.WaitForResultUntil(30*time.Second, func() (bool, error) {
			err := leafReads(ctx, js, leaf.ClientURL(), stream)
			return err == nil, err
		}, func(err error) { t.Fatalf("the leaf %s: %v", what, err) })
	}
	reads("before the compaction")

	// Four readers come and go.
	consumer := "$JS.API.CONSUMER.MSG.NEXT." + stream + ".r0"
	var gone []string
	for i := range 4 {
		name, err := s.ReaderAccount(readerTestSpec(fmt.Sprintf("r%d", i)))
		must.NoError(t, err)
		gone = append(gone, name)
	}
	bus, err := s.server().LookupAccount(AccountName)
	must.NoError(t, err)
	must.True(t, bus.IsExportService(consumer))

	// Retired: four, more than the two live ones, and due.
	now := time.Now().Add(time.Minute)
	s.sweepReaders(now)
	for _, name := range gone {
		_, err := s.server().LookupAccount(name)
		must.Error(t, err, must.Sprintf("the retired account %s is still there", name))
	}
	bus, err = s.server().LookupAccount(AccountName)
	must.NoError(t, err)
	must.False(t, bus.IsExportService(consumer), must.Sprint("the bus account still exports to the retired accounts"))
	reads("after the compaction")

	// Not again within the interval: retired, not dropped.
	gone = gone[:0]
	for i := range 4 {
		name, err := s.ReaderAccount(readerTestSpec(fmt.Sprintf("r%d", i)))
		must.NoError(t, err)
		gone = append(gone, name)
	}
	s.sweepReaders(now.Add(2 * time.Minute))
	for _, name := range gone {
		_, err := s.server().LookupAccount(name)
		must.NoError(t, err)
		must.Eq(t, 0, busImports(t, s, name))
	}
}

// TestBus_ReaderAccountMemory measures the heap a reader account keeps:
// made, retired and after the compaction. It makes as many accounts as
// NORCA_BUS_READER_ACCOUNTS says (skipped when unset), each like the account
// of a workload identity: the Job events live, a consumer of the event and
// of the Consul KV change stream, and the information.
func TestBus_ReaderAccountMemory(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("NORCA_BUS_READER_ACCOUNTS"))
	if n <= 0 {
		t.Skip("NORCA_BUS_READER_ACCOUNTS is not set")
	}
	c := testConfig(t, "s1")
	// The first sweep retires them all, the second drops them.
	c.ReaderCompactThreshold = n + 1
	s := startServer(t, c, newTestAuthz().authorize)
	s.setReaderRetirement(time.Minute, 0)
	heap := func() int64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return int64(m.HeapAlloc)
	}

	base := heap()
	start := time.Now()
	for i := range n {
		reader := fmt.Sprintf("w%d", i)
		_, err := s.ReaderAccount(&ReaderSpec{
			Reader:   reader,
			Subjects: []string{"norca.global.*.Job"},
			Consumers: []ConsumerRef{
				{Stream: EventStreamName("global"), Name: reader},
				{Stream: ConsulKVChangesStreamName("global"), Name: reader},
			},
			Info: true,
		})
		must.NoError(t, err)
		if (i+1)%(max(n/4, 1)) == 0 {
			t.Logf("%d accounts made in %s", i+1, time.Since(start))
		}
	}
	made := heap()

	now := time.Now().Add(time.Minute)
	start = time.Now()
	s.sweepReaders(now)
	t.Logf("%d accounts retired in %s", n, time.Since(start))
	retired := heap()

	s.setReaderCompactThreshold(1)
	start = time.Now()
	s.sweepReaders(now)
	t.Logf("compacted in %s", time.Since(start))
	compacted := heap()

	per := func(h int64) int64 { return (h - base) / int64(n) }
	t.Logf("heap per reader account: made %d B, retired %d B, after the compaction %d B (base %d B, %d accounts)",
		per(made), per(retired), per(compacted), base, n)
	must.Less(t, retired, compacted)
}

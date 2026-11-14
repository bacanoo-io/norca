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

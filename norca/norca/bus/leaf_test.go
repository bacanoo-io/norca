// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package bus

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/testutil"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"
)

// leafAuthz grants the leaf token "leafer" a reader account with a leaf
// spec; any other leaf token, and a client with the leaf token, are refused.
type leafAuthz struct {
	l     sync.Mutex
	s     *Server
	spec  *ReaderSpec
	other bool // the leaf token also works as a client token
}

// set changes the bus and the spec of the authorizer; the bus may call
// authorize at any time.
func (a *leafAuthz) set(s *Server, spec *ReaderSpec) {
	a.l.Lock()
	defer a.l.Unlock()
	if s != nil {
		a.s = s
	}
	a.spec = spec
}

func (a *leafAuthz) authorize(c Credentials) (*Grant, error) {
	a.l.Lock()
	defer a.l.Unlock()
	if c.Token != "leafer" {
		return nil, errors.New("unknown token")
	}
	if !c.Leaf {
		if a.other {
			return &Grant{Kind: KindToken, Name: "leafer", Subscribe: []string{"norca.>"}}, nil
		}
		return nil, errors.New("a leaf token")
	}
	name, err := a.s.ReaderAccount(a.spec)
	if err != nil {
		return nil, err
	}
	var publish []string
	for _, c := range a.spec.Consumers {
		publish = append(publish, a.spec.ConsumerSubjects(c)...)
	}
	return &Grant{Kind: KindLeaf, Name: "leafer", Account: name, Publish: publish, Subscribe: []string{">"}}, nil
}

// startLeaf starts a NATS server with JetStream of its own that attaches to
// the bus as a leaf node with token.
func startLeaf(t *testing.T, busPort int, token string) *natsserver.Server {
	t.Helper()
	store, err := os.MkdirTemp("", "norca-leaf-test")
	must.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(store) })
	u, err := url.Parse("nats-leaf://" + token + "@127.0.0.1:" + strconv.Itoa(busPort))
	must.NoError(t, err)
	opts := &natsserver.Options{
		ServerName: fmt.Sprintf("leaf-%s-%d", token, time.Now().UnixNano()),
		Host:       "127.0.0.1",
		Port:       -1,
		JetStream:  true,
		StoreDir:   store,
		NoSigs:     true,
		LeafNode: natsserver.LeafNodeOpts{
			ReconnectInterval: 100 * time.Millisecond,
			Remotes:           []*natsserver.RemoteLeafOpts{{URLs: []*url.URL{u}}},
		},
	}
	ns, err := natsserver.NewServer(opts)
	must.NoError(t, err)
	go ns.Start()
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	must.True(t, ns.ReadyForConnections(10*time.Second))
	return ns
}

func waitLeaf(t *testing.T, ns *natsserver.Server, n int) {
	t.Helper()
	testutil.WaitForResult(func() (bool, error) {
		return ns.NumLeafNodes() == n, fmt.Errorf("%d leaf connections, want %d", ns.NumLeafNodes(), n)
	}, func(err error) { t.Fatal(err) })
}

// A NATS server attached as a leaf node reads its subjects live and its
// consumer through a JetStream domain, and reaches nothing else.
func TestBus_LeafNode(t *testing.T) {
	ci.Parallel(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	authz := &leafAuthz{}
	c := testConfig(t, "s1")
	c.LeafNodes = LeafNodesConfig{Enabled: true, Port: ci.PortAllocator.Grab(1)[0]}
	s := startServer(t, c, authz.authorize)
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
	for _, name := range []string{"leaf-leafer", "other"} {
		_, err := js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{
			Durable: name, FilterSubject: "norca.global.*.Job", AckPolicy: jetstream.AckExplicitPolicy,
		})
		must.NoError(t, err)
	}
	authz.set(s, &ReaderSpec{
		Reader:    "leaf-leafer",
		Subjects:  []string{"norca.global.*.Job"},
		Consumers: []ConsumerRef{{Stream: stream, Name: "leaf-leafer"}},
		Domain:    "norca-global",
		Leaf:      true,
	})

	leaf := startLeaf(t, c.LeafNodes.Port, "leafer")
	waitLeaf(t, s.server(), 1)
	waitLeaf(t, leaf, 1)

	// A client of the leaf's own NATS.
	app, err := nats.Connect(leaf.ClientURL())
	must.NoError(t, err)
	defer app.Close()
	live, err := app.SubscribeSync("norca.>")
	must.NoError(t, err)
	must.NoError(t, app.Flush())
	// The interest of the leaf reaches the bus.
	testutil.WaitForResult(func() (bool, error) {
		_, err := js.Publish(ctx, "norca.global.default.Job", []byte("probe"))
		if err != nil {
			return false, err
		}
		_, err = live.NextMsg(200 * time.Millisecond)
		return err == nil, err
	}, func(err error) { t.Fatal(err) })

	_, err = js.Publish(ctx, "norca.global.default.Node", []byte("node"))
	must.NoError(t, err)
	_, err = js.Publish(ctx, "norca.global.default.Job", []byte("job"))
	must.NoError(t, err)
	msg, err := live.NextMsg(5 * time.Second)
	must.NoError(t, err)
	must.Eq(t, "job", string(msg.Data))
	_, err = live.NextMsg(300 * time.Millisecond)
	must.ErrorIs(t, err, nats.ErrTimeout)

	// Its consumer, through the domain: the leaf's NATS answers $JS.API
	// with its own JetStream.
	ljs, err := jetstream.NewWithDomain(app, "norca-global")
	must.NoError(t, err)
	var cons jetstream.Consumer
	testutil.WaitForResult(func() (bool, error) {
		cons, err = ljs.Consumer(ctx, stream, "leaf-leafer")
		return err == nil, err
	}, func(err error) { t.Fatal(err) })
	batch, err := cons.Fetch(10, jetstream.FetchMaxWait(2*time.Second))
	must.NoError(t, err)
	var got []string
	for m := range batch.Messages() {
		got = append(got, string(m.Data()))
		must.NoError(t, m.Ack())
	}
	must.NoError(t, batch.Error())
	must.Eq(t, []string{"probe", "job"}, got[len(got)-2:])
	must.NoError(t, app.Flush())
	testutil.WaitForResult(func() (bool, error) {
		bc, err := js.Consumer(ctx, stream, "leaf-leafer")
		if err != nil {
			return false, err
		}
		if info := bc.CachedInfo(); info.NumAckPending != 0 || info.NumPending != 0 {
			return false, fmt.Errorf("pending %d, ack pending %d", info.NumPending, info.NumAckPending)
		}
		return true, nil
	}, func(err error) { t.Fatal(err) })

	// Nothing else: no other consumer, no stream, no consumer create, and
	// nothing it publishes reaches the bus account.
	short, cancelShort := context.WithTimeout(ctx, time.Second)
	defer cancelShort()
	_, err = ljs.Consumer(short, stream, "other")
	must.Error(t, err)
	_, err = ljs.Stream(short, stream)
	must.Error(t, err)
	_, err = ljs.CreateOrUpdateConsumer(short, stream, jetstream.ConsumerConfig{Durable: "mine"})
	must.Error(t, err)
	leaked, err := nc.SubscribeSync("norca.>")
	must.NoError(t, err)
	must.NoError(t, nc.Flush())
	must.NoError(t, app.Publish("norca.global.default.Job", []byte("forged")))
	must.NoError(t, app.Flush())
	_, err = leaked.NextMsg(500 * time.Millisecond)
	must.ErrorIs(t, err, nats.ErrTimeout)

	// The account takes one leaf: a second leaf with the token is refused.
	// Its attempts, one every 100ms, leave nothing behind on the bus.
	second := startLeaf(t, c.LeafNodes.Port, "leafer")
	time.Sleep(time.Second)
	must.Eq(t, 0, second.NumLeafNodes())
	must.Eq(t, 1, leaf.NumLeafNodes())
	must.Eq(t, 1, s.server().NumLeafNodes())
	second.Shutdown()

	// A client with the leaf token: refused as a client (no leaf grant).
	_, err = nats.Connect(s.ClientURL(), nats.Token("leafer"), nats.MaxReconnects(0))
	must.Error(t, err)

	// An unknown token attaches no leaf.
	stranger := startLeaf(t, c.LeafNodes.Port, "stranger")
	time.Sleep(500 * time.Millisecond)
	must.Eq(t, 0, stranger.NumLeafNodes())
	stranger.Shutdown()
	time.Sleep(200 * time.Millisecond)

	// Revalidate keeps the leaf while its grant is the same, and closes it
	// when the grant changes.
	s.Revalidate()
	must.Eq(t, 1, leaf.NumLeafNodes())
	moved := &ReaderSpec{
		Reader:   "leaf-leafer",
		Subjects: []string{"norca.global.*.Node"},
		Domain:   "norca-global",
		Leaf:     true,
	}
	authz.set(nil, moved)
	s.Revalidate()
	// It reconnects into the account of the new spec.
	testutil.WaitForResult(func() (bool, error) {
		leafz, err := s.server().Leafz(nil)
		if err != nil {
			return false, err
		}
		if len(leafz.Leafs) != 1 || leafz.Leafs[0].Account != ReaderAccountName(moved) {
			return false, fmt.Errorf("leafs %+v", leafz.Leafs)
		}
		return true, nil
	}, func(err error) { t.Fatal(err) })
}

// A leaf grant is for a leaf connection only, and a client grant for a
// client only.
func TestBus_LeafGrantKinds(t *testing.T) {
	ci.Parallel(t)
	c := testConfig(t, "s1")
	c.LeafNodes = LeafNodesConfig{Enabled: true, Port: ci.PortAllocator.Grab(1)[0]}
	// The client token answers a leaf with a client grant.
	startServer(t, c, func(cr Credentials) (*Grant, error) {
		if cr.Token == "client" {
			return &Grant{Kind: KindToken, Name: "client", Subscribe: []string{"norca.>"}}, nil
		}
		return nil, errors.New("unknown")
	})
	leaf := startLeaf(t, c.LeafNodes.Port, "client")
	time.Sleep(time.Second)
	must.Eq(t, 0, leaf.NumLeafNodes())

	// The leaf listener is off unless enabled.
	c2 := testConfig(t, "s2")
	s2 := startServer(t, c2, newTestAuthz().authorize)
	must.Eq(t, 0, s2.opts.LeafNode.Port)
}

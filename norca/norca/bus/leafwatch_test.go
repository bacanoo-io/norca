// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package bus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
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

// logBuffer collects the log of a bus.
type logBuffer struct {
	l sync.Mutex
	b bytes.Buffer
}

func (s *logBuffer) Write(p []byte) (int, error) {
	s.l.Lock()
	defer s.l.Unlock()
	return s.b.Write(p)
}

func (s *logBuffer) String() string {
	s.l.Lock()
	defer s.l.Unlock()
	return s.b.String()
}

// leafSpec is the reader spec of the leaf tests: the Job events live and
// the consumer leaf-leafer.
func leafSpec(stream string) *ReaderSpec {
	return &ReaderSpec{
		Reader:    "leaf-leafer",
		Subjects:  []string{"norca.global.*.Job"},
		Consumers: []ConsumerRef{{Stream: stream, Name: "leaf-leafer"}},
		Domain:    "norca-global",
		Leaf:      true,
	}
}

// In a cluster the account of a leaf exists only on the server it attached
// to. The other server logs no error for the subscriptions of the account it
// does not know, and the leaf can move to it.
func TestBus_LeafNodeCluster(t *testing.T) {
	ci.Parallel(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	var servers []*Server
	var authzs []*leafAuthz
	var addrs []string
	var configs []*Config
	logs := []*logBuffer{{}, {}}
	for i := range 2 {
		c := testConfig(t, fmt.Sprintf("s%d", i+1))
		c.Clustered = true
		c.LeafNodes = LeafNodesConfig{Enabled: true, Port: ci.PortAllocator.Grab(1)[0]}
		addrs = append(addrs, net.JoinHostPort(c.BindAddr, strconv.Itoa(c.ClusterPort)))
		a := &leafAuthz{}
		s, err := New(c, hclog.New(&hclog.LoggerOptions{Level: hclog.Warn, Output: logs[i]}), a.authorize)
		must.NoError(t, err)
		t.Cleanup(s.Shutdown)
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
	spec := leafSpec(stream)
	for _, a := range authzs {
		a.set(nil, spec)
	}

	// A client of the leaf's NATS with many subscriptions receives a Job
	// event live and fetches one from its consumer.
	read := func(leafURL string) {
		app, err := nats.Connect(leafURL)
		must.NoError(t, err)
		defer app.Close()
		live, err := app.SubscribeSync("norca.>")
		must.NoError(t, err)
		for i := range 50 {
			_, err := app.SubscribeSync(fmt.Sprintf("norca.global.ns%d.Job", i))
			must.NoError(t, err)
		}
		must.NoError(t, app.Flush())
		testutil.WaitForResult(func() (bool, error) {
			if _, err := js.Publish(ctx, "norca.global.default.Job", []byte("job")); err != nil {
				return false, err
			}
			_, err := live.NextMsg(300 * time.Millisecond)
			return err == nil, err
		}, func(err error) { t.Fatal(err) })
		ljs, err := jetstream.NewWithDomain(app, "norca-global")
		must.NoError(t, err)
		var cons jetstream.Consumer
		testutil.WaitForResult(func() (bool, error) {
			cons, err = ljs.Consumer(ctx, stream, "leaf-leafer")
			return err == nil, err
		}, func(err error) { t.Fatal(err) })
		batch, err := cons.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
		must.NoError(t, err)
		n := 0
		for m := range batch.Messages() {
			must.NoError(t, m.Ack())
			n++
		}
		must.NoError(t, batch.Error())
		must.Eq(t, 1, n)
	}

	// The leaf on the first server; the stream is written through the second.
	leaf := startLeaf(t, configs[0].LeafNodes.Port, "leafer")
	waitLeaf(t, leaf, 1)
	read(leaf.ClientURL())
	time.Sleep(500 * time.Millisecond)
	_, err = servers[1].server().LookupAccount(ReaderAccountName(spec))
	must.Error(t, err, must.Sprint("the second server knows the account of the leaf"))
	must.StrNotContains(t, logs[1].String(), "[ERROR]")

	// The leaf moves to the second server, which makes the account itself.
	leaf.Shutdown()
	leaf.WaitForShutdown()
	leaf = startLeaf(t, configs[1].LeafNodes.Port, "leafer")
	waitLeaf(t, leaf, 1)
	read(leaf.ClientURL())
	time.Sleep(500 * time.Millisecond)
	for i, l := range logs {
		must.StrNotContains(t, l.String(), "[ERROR]", must.Sprintf("log of server %d", i+1))
	}
}

// A leaf that sends fetches without end is cut off: the bus account keeps
// the way back of every fetch, for the response threshold, and their number
// is bounded by closing the leaf.
func TestBus_LeafFetchFlood(t *testing.T) {
	ci.Parallel(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	authz := &leafAuthz{}
	c := testConfig(t, "s1")
	c.LeafNodes = LeafNodesConfig{Enabled: true, Port: ci.PortAllocator.Grab(1)[0]}
	log := &logBuffer{}
	s, err := New(c, hclog.New(&hclog.LoggerOptions{Level: hclog.Warn, Output: log}), authz.authorize)
	must.NoError(t, err)
	t.Cleanup(s.Shutdown)
	// Short limits for the test, set before the account of the leaf is made.
	const limit = 2000
	s.setReaderLimits(limit, 3*time.Second, 4*time.Second)
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
	_, err = js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable: "leaf-leafer", FilterSubject: "norca.global.*.Job", AckPolicy: jetstream.AckExplicitPolicy,
		MaxWaiting: 16, MaxRequestExpires: ReaderMaxFetchExpires,
	})
	must.NoError(t, err)
	spec := leafSpec(stream)
	authz.set(s, spec)

	leaf := startLeaf(t, c.LeafNodes.Port, "leafer")
	waitLeaf(t, leaf, 1)
	app, err := nats.Connect(leaf.ClientURL(), nats.MaxReconnects(-1))
	must.NoError(t, err)
	defer app.Close()
	_, err = app.SubscribeSync("_INBOX.flood.>")
	must.NoError(t, err)
	must.NoError(t, app.Flush())
	time.Sleep(300 * time.Millisecond)

	bus, err := s.server().LookupAccount(AccountName)
	must.NoError(t, err)
	next := "$JS.norca-global.API.CONSUMER.MSG.NEXT." + stream + ".leaf-leafer"
	fetch := func(n int) {
		for i := range n {
			must.NoError(t, app.PublishRequest(next, fmt.Sprintf("_INBOX.flood.%d", i), []byte(`{"batch":1,"expires":1000000000}`)))
		}
		must.NoError(t, app.Flush())
	}

	// Below the limit: the ways back stay for the response threshold and go
	// by themselves; the leaf stays.
	fetch(500)
	testutil.WaitForResult(func() (bool, error) {
		n := bus.NumPendingAllResponses()
		return n >= 500, fmt.Errorf("%d ways back", n)
	}, func(err error) { t.Fatal(err) })
	testutil.WaitForResult(func() (bool, error) {
		n := bus.NumPendingAllResponses()
		return n == 0, fmt.Errorf("%d ways back after the response threshold", n)
	}, func(err error) { t.Fatal(err) })
	must.Eq(t, 1, leaf.NumLeafNodes())
	must.StrNotContains(t, log.String(), "too many of its requests")

	// Above the limit: the leaf is closed and refused for the ban time, and
	// its ways back are gone.
	fetch(6000)
	testutil.WaitForResult(func() (bool, error) {
		if !strings.Contains(log.String(), "too many of its requests") {
			return false, errors.New("the leaf was not cut off")
		}
		return true, nil
	}, func(err error) { t.Fatal(err) })
	must.False(t, s.readerBanned(ReaderAccountName(spec)).IsZero())
	must.Eq(t, 0, s.server().NumLeafNodes())
	testutil.WaitForResult(func() (bool, error) {
		n := bus.NumPendingAllResponses()
		return n <= limit, fmt.Errorf("%d ways back after the leaf was closed", n)
	}, func(err error) { t.Fatal(err) })

	// After the ban the leaf attaches again and reads its consumer.
	waitLeaf(t, leaf, 1)
	must.True(t, s.readerBanned(ReaderAccountName(spec)).IsZero())
	_, err = js.Publish(ctx, "norca.global.default.Job", []byte("job"))
	must.NoError(t, err)
	ljs, err := jetstream.NewWithDomain(app, "norca-global")
	must.NoError(t, err)
	var cons jetstream.Consumer
	testutil.WaitForResult(func() (bool, error) {
		cons, err = ljs.Consumer(ctx, stream, "leaf-leafer")
		return err == nil, err
	}, func(err error) { t.Fatal(err) })
	testutil.WaitForResult(func() (bool, error) {
		batch, err := cons.Fetch(1, jetstream.FetchMaxWait(time.Second))
		if err != nil {
			return false, err
		}
		for m := range batch.Messages() {
			return true, m.Ack()
		}
		return false, errors.New("no message")
	}, func(err error) { t.Fatal(err) })
}

// Concurrent calls for one spec make the account once.
func TestBus_ReaderAccountConcurrent(t *testing.T) {
	ci.Parallel(t)
	s := startServer(t, testConfig(t, "s1"), newTestAuthz().authorize)
	spec := leafSpec(EventStreamName("global"))
	var wg sync.WaitGroup
	names := make([]string, 8)
	errs := make([]error, 8)
	for i := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			names[i], errs[i] = s.ReaderAccount(spec)
		}()
	}
	wg.Wait()
	for i := range names {
		must.NoError(t, errs[i])
		must.Eq(t, ReaderAccountName(spec), names[i])
	}
	acc, err := s.server().LookupAccount(names[0])
	must.NoError(t, err)
	// As many imports as an account made by one call.
	solo := leafSpec(EventStreamName("global"))
	solo.Reader = "leaf-solo"
	soloName, err := s.ReaderAccount(solo)
	must.NoError(t, err)
	soloAcc, err := s.server().LookupAccount(soloName)
	must.NoError(t, err)
	must.Eq(t, soloAcc.NumServiceImports(), acc.NumServiceImports())
}

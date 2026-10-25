// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package natsbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// runNATS starts an embedded NATS server, with JetStream if js is set.
func runNATS(t *testing.T, js bool) *server.Server {
	t.Helper()
	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		NoLog:     true,
		NoSigs:    true,
		JetStream: js,
		StoreDir:  t.TempDir(),
	}
	s, err := server.NewServer(opts)
	must.NoError(t, err)
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatal("the NATS server did not start")
	}
	t.Cleanup(func() {
		s.Shutdown()
		s.WaitForShutdown()
	})
	return s
}

func connect(t *testing.T, s *server.Server) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(s.ClientURL(), nats.ReconnectBufSize(-1))
	must.NoError(t, err)
	t.Cleanup(nc.Close)
	return nc
}

func testLogger(t *testing.T) *slog.Logger {
	if os.Getenv("NATSBRIDGE_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// streamCall records one subscription to the fake source.
type streamCall struct {
	Index uint64
	Exact bool
}

// fakeSource is an in-memory event stream with an event buffer that can be
// truncated, following the semantics of the server: an exact subscription
// below the oldest buffered index fails with ErrIndexGone, index 0 starts
// at the newest batch.
type fakeSource struct {
	mu      sync.Mutex
	batches []Batch
	oldest  uint64 // batches below this index were dropped
	changed chan struct{}
	calls   []streamCall
	cancels []context.CancelFunc
	failErr error
}

func newFakeSource() *fakeSource {
	return &fakeSource{changed: make(chan struct{}), oldest: 1}
}

// add appends a batch with one event per type; the events are of topic Job
// with the key given. It returns the index of the batch.
func (f *fakeSource) add(topic, key string, types ...string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := uint64(100)
	if n := len(f.batches); n > 0 {
		index = f.batches[n-1].Index + 1
	}
	batch := Batch{Index: index}
	for _, typ := range types {
		raw := fmt.Sprintf(`{"Topic":%q,"Type":%q,"Key":%q,"Namespace":"default","FilterKeys":null,"Index":%d,"Payload":{"Big":1700000000123456789}}`,
			topic, typ, key, index)
		var ev Event
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			panic(err)
		}
		ev.Raw = json.RawMessage(raw)
		batch.Events = append(batch.Events, ev)
	}
	f.batches = append(f.batches, batch)
	close(f.changed)
	f.changed = make(chan struct{})
	return index
}

// truncate drops the batches below index from the buffer.
func (f *fakeSource) truncate(index uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.oldest = index
}

// drop ends the open streams, like a server that closes the connection.
func (f *fakeSource) drop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cancels {
		c()
	}
	f.cancels = nil
}

func (f *fakeSource) setFail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failErr = err
}

func (f *fakeSource) Calls() []streamCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]streamCall(nil), f.calls...)
}

func (f *fakeSource) Stream(ctx context.Context, index uint64, exact bool) (<-chan Batch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, streamCall{index, exact})
	if f.failErr != nil {
		return nil, f.failErr
	}
	if exact && index == 0 {
		return nil, errors.New("exact requires an index")
	}
	if exact && index < f.oldest {
		return nil, fmt.Errorf("%w: requested %d, oldest %d", ErrIndexGone, index, f.oldest)
	}
	// Position of the first batch to send.
	pos := len(f.batches)
	if index == 0 {
		pos = max(len(f.batches)-1, 0)
	} else {
		for i, b := range f.batches {
			if b.Index >= index && b.Index >= f.oldest {
				pos = i
				break
			}
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	f.cancels = append(f.cancels, cancel)
	ch := make(chan Batch)
	go func() {
		defer close(ch)
		defer cancel()
		for {
			f.mu.Lock()
			changed := f.changed
			var batch *Batch
			if pos < len(f.batches) {
				batch = &f.batches[pos]
				pos++
			}
			f.mu.Unlock()
			if batch == nil {
				select {
				case <-ctx.Done():
					return
				case <-changed:
					continue
				}
			}
			select {
			case <-ctx.Done():
				return
			case ch <- *batch:
			}
		}
	}()
	return ch, nil
}

// flakyPublisher fails the next n batches.
type flakyPublisher struct {
	Publisher
	mu    sync.Mutex
	fails int
}

func (p *flakyPublisher) failNext(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fails = n
}

func (p *flakyPublisher) PublishBatch(ctx context.Context, msgs []*nats.Msg) (int, error) {
	p.mu.Lock()
	fail := p.fails > 0
	if fail {
		p.fails--
	}
	p.mu.Unlock()
	if fail {
		return 0, errors.New("injected publish failure")
	}
	return p.Publisher.PublishBatch(ctx, msgs)
}

// collector records the messages of a subscription.
type collector struct {
	mu   sync.Mutex
	msgs []*nats.Msg
}

func collect(t *testing.T, nc *nats.Conn, subject string) *collector {
	t.Helper()
	c := &collector{}
	sub, err := nc.Subscribe(subject, func(m *nats.Msg) {
		c.mu.Lock()
		c.msgs = append(c.msgs, m)
		c.mu.Unlock()
	})
	must.NoError(t, err)
	must.NoError(t, nc.Flush())
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return c
}

func (c *collector) all() []*nats.Msg {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*nats.Msg(nil), c.msgs...)
}

// keys returns the Nomad-Key headers of the messages in order.
func (c *collector) keys() []string {
	var out []string
	for _, m := range c.all() {
		out = append(out, m.Header.Get(HeaderKey))
	}
	return out
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(f),
		wait.Timeout(15*time.Second),
		wait.Gap(20*time.Millisecond),
	), must.Sprint(what))
}

// runBridge runs b until the returned stop function is called.
func runBridge(b *Bridge) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = b.Run(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

func fastOptions(region string) Options {
	return Options{
		Region:                 region,
		CheckpointInterval:     20 * time.Millisecond,
		CheckpointIdleInterval: 50 * time.Millisecond,
		RetryBase:              10 * time.Millisecond,
		RetryMax:               50 * time.Millisecond,
	}
}

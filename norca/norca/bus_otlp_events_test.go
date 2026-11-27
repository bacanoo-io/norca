// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/telemetry"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"
)

// fakeEventExporter records the exports; the first failures of them fail.
type fakeEventExporter struct {
	topics []string

	mu       sync.Mutex
	failures int
	attempts [][]telemetry.Event
	exported []telemetry.Event
}

var _ EventExporter = (*fakeEventExporter)(nil)

func (f *fakeEventExporter) Topics() []string { return slices.Clone(f.topics) }

func (f *fakeEventExporter) Export(_ context.Context, events []telemetry.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts = append(f.attempts, slices.Clone(events))
	if f.failures > 0 {
		f.failures--
		return errors.New("receiver down")
	}
	f.exported = append(f.exported, events...)
	return nil
}

// keys returns the keys of the exported events of a topic.
func (f *fakeEventExporter) keys(topic string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, ev := range f.exported {
		if ev.Topic == topic {
			out = append(out, ev.Key)
		}
	}
	return out
}

func (f *fakeEventExporter) attemptKeys() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.attempts))
	for i, a := range f.attempts {
		for _, ev := range a {
			out[i] = append(out[i], ev.Key)
		}
	}
	return out
}

// waitOTLPConsumers waits until the durable consumers of the export exist
// on both event streams: the export starts with the events after them.
func waitOTLPConsumers(t *testing.T, js jetstream.JetStream) {
	t.Helper()
	waitBus(t, func() (bool, error) {
		for _, name := range []string{bus.EventStreamName("global"), bus.AdminEventStreamName("global")} {
			if _, err := js.Consumer(t.Context(), name, otlpEventsConsumer); err != nil {
				return false, err
			}
		}
		return true, nil
	})
}

// The leader exports the events of the default topics from both event
// streams; the topics that may carry secrets stay out unless listed.
func TestBusEventsOTLP_Export(t *testing.T) {
	ci.Parallel(t)

	exp := &fakeEventExporter{topics: telemetry.DefaultEventTopics}
	s1, cleanup := TestServer(t, func(c *Config) {
		enableBus(t)(c)
		c.EventExport = exp
	})
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	js := busJS(t, s1)
	waitOTLPConsumers(t, js)

	st := s1.fsm.State()
	job := mock.Job()
	node := mock.Node()
	must.NoError(t, st.UpsertACLPolicies(structs.ACLPolicyUpsertRequestType, 1100, []*structs.ACLPolicy{mock.ACLPolicy()}))
	must.NoError(t, st.UpsertNode(structs.NodeRegisterRequestType, 1101, node))
	must.NoError(t, st.UpsertJob(structs.JobRegisterRequestType, 1102, nil, job))

	waitBus(t, func() (bool, error) {
		return slices.Contains(exp.keys("Job"), job.ID) && slices.Contains(exp.keys("Node"), node.ID), nil
	})
	// the ACL policy came before both, through the admin stream
	time.Sleep(500 * time.Millisecond)
	must.SliceEmpty(t, exp.keys("ACLPolicy"), must.Sprint("an ACL event was exported without being listed"))

	exp.mu.Lock()
	var ev telemetry.Event
	for _, e := range exp.exported {
		if e.Topic == "Job" && e.Key == job.ID {
			ev = e
		}
	}
	exp.mu.Unlock()
	must.Eq(t, "JobRegistered", ev.Type)
	must.Eq(t, job.Namespace, ev.Namespace)
	must.Eq(t, uint64(1102), ev.Index)
	must.False(t, ev.Time.IsZero())
	var payload struct{ Job struct{ ID string } }
	must.NoError(t, json.Unmarshal(ev.Payload, &payload), must.Sprint("the body is the payload of the event"))
	must.Eq(t, job.ID, payload.Job.ID)

	// every export was acknowledged
	cons, err := js.Consumer(t.Context(), bus.EventStreamName("global"), otlpEventsConsumer)
	must.NoError(t, err)
	info, err := cons.Info(t.Context())
	must.NoError(t, err)
	must.Eq(t, 0, info.NumAckPending)
}

// While the receiver is down the export retries the batch it holds and
// fetches nothing more; an export that stops (a leader change, a restart)
// continues after the last acknowledged event.
func TestBusEventsOTLP_BackpressureAndResume(t *testing.T) {
	ci.Parallel(t)

	// the server runs no export of its own: the test runs the loop
	s1, cleanup := TestServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	js := busJS(t, s1)
	waitBus(t, func() (bool, error) {
		_, err := js.Stream(t.Context(), bus.EventStreamName("global"))
		return err == nil, err
	})
	stream := s1.otlpEventStreams([]string{"Job"})[0]
	must.Eq(t, bus.EventStreamName("global"), stream.name)
	must.Eq(t, []string{"norca.global.*.Job"}, stream.filters)

	run := func(exp *fakeEventExporter) (cancel func()) {
		ctx, cancelCtx := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- s1.exportBusEventsOTLP(ctx, js, stream, exp) }()
		return func() {
			cancelCtx()
			select {
			case err := <-done:
				must.NoError(t, err)
			case <-time.After(30 * time.Second):
				t.Fatal("the export did not stop")
			}
		}
	}
	consumer := func() *jetstream.ConsumerInfo {
		cons, err := js.Consumer(t.Context(), stream.name, otlpEventsConsumer)
		must.NoError(t, err)
		info, err := cons.Info(t.Context())
		must.NoError(t, err)
		return info
	}
	st := s1.fsm.State()
	upsertJob := func(index uint64) string {
		job := mock.Job()
		must.NoError(t, st.UpsertJob(structs.JobRegisterRequestType, index, nil, job))
		return job.ID
	}

	first := &fakeEventExporter{topics: []string{"Job"}, failures: 2}
	stop := run(first)
	waitBus(t, func() (bool, error) {
		_, err := js.Consumer(t.Context(), stream.name, otlpEventsConsumer)
		return err == nil, err
	})
	a := upsertJob(2000)
	waitBus(t, func() (bool, error) { return len(first.attemptKeys()) > 0, nil })
	// held while failing: a new event waits in the stream
	b := upsertJob(2001)
	waitBus(t, func() (bool, error) { return len(first.keys("Job")) == 2, nil })
	stop()

	must.Eq(t, [][]string{{a}, {a}, {a}, {b}}, first.attemptKeys(),
		must.Sprint("the failing batch is retried alone; the next one is fetched after it is out"))
	must.Eq(t, []string{a, b}, first.keys("Job"))
	info := consumer()
	must.Eq(t, 0, info.NumAckPending)
	must.Eq(t, uint64(0), info.NumPending)

	// an event while no export runs; the next export starts after b
	c := upsertJob(2002)
	second := &fakeEventExporter{topics: []string{"Job"}}
	stop = run(second)
	waitBus(t, func() (bool, error) { return slices.Contains(second.keys("Job"), c), nil })
	time.Sleep(500 * time.Millisecond)
	stop()
	must.Eq(t, []string{c}, second.keys("Job"), must.Sprint("the export resumes after the last acknowledged event"))

	// the leadership ends while an export fails: the held event goes back
	// to JetStream, and the next leader exports it at once, not after the
	// acknowledgement wait
	failing := &fakeEventExporter{topics: []string{"Job"}, failures: 1000}
	stop = run(failing)
	d := upsertJob(2003)
	waitBus(t, func() (bool, error) { return len(failing.attemptKeys()) > 0, nil })
	stop()
	third := &fakeEventExporter{topics: []string{"Job"}}
	start := time.Now()
	stop = run(third)
	waitBus(t, func() (bool, error) { return slices.Contains(third.keys("Job"), d), nil })
	stop()
	must.Less(t, otlpEventsAckWait/2, time.Since(start))
	must.SliceEmpty(t, failing.keys("Job"))
}

// A leader with the export off deletes the consumers of the export, and an
// export of other topics than its consumer was made for starts with a new
// consumer at the new events: the events of the pause are not sent.
func TestBusEventsOTLP_Consumers(t *testing.T) {
	ci.Parallel(t)

	// the server runs no export of its own: the test runs the loops
	s1, cleanup := TestServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)
	js := busJS(t, s1)
	waitBus(t, func() (bool, error) {
		for _, name := range []string{bus.EventStreamName("global"), bus.AdminEventStreamName("global")} {
			if _, err := js.Stream(t.Context(), name); err != nil {
				return false, err
			}
		}
		return true, nil
	})
	run := func(fn func(context.Context) error) (stop func()) {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- fn(ctx) }()
		return func() {
			cancel()
			select {
			case err := <-done:
				must.NoError(t, err)
			case <-time.After(30 * time.Second):
				t.Fatal("the loop did not stop")
			}
		}
	}
	consumerFilters := func() ([]string, error) {
		cons, err := js.Consumer(t.Context(), bus.EventStreamName("global"), otlpEventsConsumer)
		if err != nil {
			return nil, err
		}
		cfg := cons.CachedInfo().Config
		if len(cfg.FilterSubjects) == 0 {
			return []string{cfg.FilterSubject}, nil
		}
		return cfg.FilterSubjects, nil
	}
	st := s1.fsm.State()

	// an export of Job, then a pause with a Job and an Evaluation event
	jobs := &fakeEventExporter{topics: []string{"Job"}}
	jobStream := s1.otlpEventStreams(jobs.topics)[0]
	stop := run(func(ctx context.Context) error { return s1.exportBusEventsOTLP(ctx, js, jobStream, jobs) })
	waitBus(t, func() (bool, error) { _, err := consumerFilters(); return err == nil, err })
	stop()
	paused := mock.Job()
	must.NoError(t, st.UpsertJob(structs.JobRegisterRequestType, 3000, nil, paused))
	pausedEval := mock.Eval()
	must.NoError(t, st.UpsertEvals(structs.EvalUpdateRequestType, 3001, []*structs.Evaluation{pausedEval}))

	// the topics change: a new consumer, from the new events on
	both := &fakeEventExporter{topics: []string{"Job", "Evaluation"}}
	bothStream := s1.otlpEventStreams(both.topics)[0]
	stop = run(func(ctx context.Context) error { return s1.exportBusEventsOTLP(ctx, js, bothStream, both) })
	waitBus(t, func() (bool, error) {
		filters, err := consumerFilters()
		return err == nil && sameStrings(filters, bothStream.filters), err
	})
	eval := mock.Eval()
	must.NoError(t, st.UpsertEvals(structs.EvalUpdateRequestType, 3002, []*structs.Evaluation{eval}))
	waitBus(t, func() (bool, error) { return slices.Contains(both.keys("Evaluation"), eval.ID), nil })
	stop()
	must.SliceNotContains(t, both.keys("Evaluation"), pausedEval.ID)
	must.SliceNotContains(t, both.keys("Job"), paused.ID)

	// the export is off: the leader deletes the consumers of both streams
	stop = run(func(ctx context.Context) error { return s1.removeOTLPEventConsumers(ctx, js) })
	waitBus(t, func() (bool, error) {
		for _, name := range []string{bus.EventStreamName("global"), bus.AdminEventStreamName("global")} {
			if _, err := js.Consumer(t.Context(), name, otlpEventsConsumer); !errors.Is(err, jetstream.ErrConsumerNotFound) {
				return false, err
			}
		}
		return true, nil
	})
	stop()
}

func TestOTLPEventChunk(t *testing.T) {
	ci.Parallel(t)

	msg := func(size int) jetstream.Msg { return fakeMsg{data: make([]byte, size)} }
	half := telemetry.MaxEventBatchBytes / 2
	must.Eq(t, 3, otlpEventChunk([]jetstream.Msg{msg(10), msg(10), msg(10)}))
	must.Eq(t, 2, otlpEventChunk([]jetstream.Msg{msg(half), msg(half), msg(1)}))
	must.Eq(t, 2, otlpEventChunk([]jetstream.Msg{msg(700 << 10), msg(700 << 10), msg(700 << 10)}))
	must.Eq(t, 2, otlpEventChunk([]jetstream.Msg{msg(3 * telemetry.MaxEventBatchBytes), msg(3 * telemetry.MaxEventBatchBytes), msg(1)}),
		must.Sprint("a payload counts as its cut body"))
}

func TestEventPayload(t *testing.T) {
	ci.Parallel(t)

	must.Eq(t, `{"Job":{"ID":"web"}}`, string(eventPayload([]byte(`{"Region":"global","Topic":"Job","Payload":{"Job":{"ID":"web"}}}`))))
	must.Eq(t, `{"Topic":"Job"}`, string(eventPayload([]byte(`{"Topic":"Job"}`))))
	must.Eq(t, `not json`, string(eventPayload([]byte(`not json`))))
}

// fakeMsg is a jetstream.Msg with data only.
type fakeMsg struct {
	jetstream.Msg
	data []byte
}

func (m fakeMsg) Data() []byte { return m.data }

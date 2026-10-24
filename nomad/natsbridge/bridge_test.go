// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package natsbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/controller"
	"github.com/hashicorp/nomad/controller/fake"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/shoenig/test/must"
)

func TestParseTopics(t *testing.T) {
	f, err := ParseTopics("Job:web, Node ,Job:api,Variables:crd/*")
	must.NoError(t, err)
	must.Eq(t, []TopicFilter{
		{Topic: "Job", Keys: []string{"web", "api"}},
		{Topic: "Node"},
		{Topic: "Variables", Keys: []string{"crd/*"}},
	}, f)
	must.Eq(t, "Job:api,Job:web,Node,Variables:crd/*", topicsString(f))

	for _, bad := range []string{"", ",", "Job:", ":x", "Jo.b"} {
		_, err := ParseTopics(bad)
		must.Error(t, err, must.Sprint(bad))
	}
}

func TestSubjectAndMessage(t *testing.T) {
	must.Eq(t, "nomad.eu_west.Job", Subject("nomad", "eu.west", "Job"))
	must.Eq(t, "norca.events.a_b.Node", Subject("norca.events", "a b", "Node"))

	raw := `{"Topic":"Job","Type":"JobRegistered","Key":"web","Namespace":"default","Index":42,"Payload":{"Job":{"SubmitTime":1700000000123456789}}}`
	var ev Event
	must.NoError(t, json.Unmarshal([]byte(raw), &ev))
	ev.Raw = []byte(raw)
	msg := eventMessage("nomad", "global", &ev)
	must.Eq(t, "nomad.global.Job", msg.Subject)
	must.Eq(t, `{"Region":"global",`+raw[1:], string(msg.Data))
	must.Eq(t, "global:42:Job:JobRegistered:default:web", msg.Header.Get(HeaderMsgID))
	must.Eq(t, "42", msg.Header.Get(HeaderIndex))
	must.Eq(t, "web", msg.Header.Get(HeaderKey))
	// Large integers keep their precision.
	must.True(t, bytes.Contains(msg.Data, []byte("1700000000123456789")))

	must.Eq(t, `{"Region":"r"}`, string(withRegion([]byte(`{}`), "r")))
	must.Eq(t, `{"Region":"r" }`, string(withRegion([]byte(`{ }`), "r")))
	must.Eq(t, `{"Region":"r","A":1}`, string(withRegion([]byte(`{"A":1}`), "r")))
}

func TestReadStream(t *testing.T) {
	input := `{}
{"Index":7,"Events":[{"Topic":"Job","Type":"JobRegistered","Key":"a","Namespace":"default","Index":7,"Payload":{"N":12345678901234567890}}]}
{"Index":9,"Events":[{"Topic":"Node","Type":"NodeRegistration","Key":"n1","Index":9,"Payload":{}},{"Topic":"Node","Type":"NodeEvent","Key":"n1","Index":9,"Payload":{}}]}
`
	ch := make(chan Batch, 10)
	readStream(context.Background(), strings.NewReader(input), ch)
	close(ch)
	var got []Batch
	for b := range ch {
		got = append(got, b)
	}
	must.Len(t, 3, got)
	must.Eq(t, uint64(7), got[0].Index)
	must.Eq(t, "a", got[0].Events[0].Key)
	must.StrContains(t, string(got[0].Events[0].Raw), "12345678901234567890")
	must.Len(t, 2, got[1].Events)
	must.Eq(t, "NodeEvent", got[1].Events[1].Type)
	must.Error(t, got[2].Err) // the stream ended
}

func newTestBridge(t *testing.T, src EventSource, pub Publisher, cp Checkpointer, opts Options) (*Bridge, *Metrics) {
	t.Helper()
	m := NewMetrics()
	b, err := New(src, pub, cp, opts, m, testLogger(t))
	must.NoError(t, err)
	return b, m
}

func TestBridge_CorePublishAndResumeAfterRestart(t *testing.T) {
	s := runNATS(t, false)
	nc := connect(t, s)
	sub := collect(t, connect(t, s), "nomad.r1.>")

	src := newFakeSource()
	src.add("Job", "j0", "JobRegistered") // before the first start
	cp := &FileCheckpoint{Path: filepath.Join(t.TempDir(), "checkpoint.json"), Region: "r1"}
	pub := &CorePublisher{Conn: nc, Timeout: 5 * time.Second}

	// The first start without a checkpoint begins with the newest batch.
	b, m := newTestBridge(t, src, pub, cp, fastOptions("r1"))
	stop := runBridge(b)
	waitFor(t, "first batch", func() bool { return len(sub.all()) == 1 })
	src.add("Job", "j1", "JobRegistered")
	i2 := src.add("Job", "j2", "JobRegistered", "JobDeregistered")
	waitFor(t, "published", func() bool { return len(sub.all()) == 4 })
	stop()

	index, found, err := cp.Load(context.Background())
	must.NoError(t, err)
	must.True(t, found)
	must.Eq(t, i2, index)
	must.Eq(t, float64(4), m.Value(MetricPublished, "topic", "Job"))
	must.Eq(t, float64(i2), m.Value(MetricCheckpointIndex))

	msg := sub.all()[1]
	must.Eq(t, "nomad.r1.Job", msg.Subject)
	must.Eq(t, "r1", msg.Header.Get(HeaderRegion))
	var payload map[string]any
	must.NoError(t, json.Unmarshal(msg.Data, &payload))
	must.Eq(t, "r1", payload["Region"])
	must.Eq(t, "JobRegistered", payload["Type"])

	// Events while the bridge is stopped are delivered after the restart,
	// nothing before them again.
	src.add("Job", "j3", "JobRegistered")
	src.add("Job", "j4", "JobRegistered")
	b, _ = newTestBridge(t, src, pub, cp, fastOptions("r1"))
	stop = runBridge(b)
	defer stop()
	waitFor(t, "resumed", func() bool { return len(sub.all()) == 6 })
	time.Sleep(100 * time.Millisecond)
	must.Eq(t, []string{"j0", "j1", "j2", "j2", "j3", "j4"}, sub.keys())
	calls := src.Calls()
	must.Eq(t, streamCall{Index: 0, Exact: false}, calls[0])
	must.Eq(t, streamCall{Index: i2 + 1, Exact: true}, calls[len(calls)-1])

	// A dropped connection resumes exactly.
	src.drop()
	src.add("Job", "j5", "JobRegistered")
	waitFor(t, "after reconnect", func() bool { return len(sub.all()) == 7 })
	must.Eq(t, "j5", sub.keys()[6])
}

func TestBridge_StartOldestAndFilters(t *testing.T) {
	s := runNATS(t, false)
	sub := collect(t, connect(t, s), "norca.>")

	src := newFakeSource()
	src.add("Job", "a", "JobRegistered", "JobDeregistered")
	src.add("Variables", "nomad/jobs/bridge/checkpoint", "VariableUpserted")
	src.add("Job", "b", "JobRegistered")

	opts := fastOptions("r1")
	opts.SubjectPrefix = "norca"
	opts.StartAt = StartOldest
	opts.EventTypes = map[string]bool{"JobRegistered": true, "VariableUpserted": true}
	opts.Skip = SkipVariables("default", "nomad/jobs/bridge/checkpoint", "nomad/jobs/bridge/lease")
	b, m := newTestBridge(t, src, &CorePublisher{Conn: connect(t, s)}, &MemoryCheckpoint{}, opts)
	stop := runBridge(b)
	defer stop()
	waitFor(t, "published", func() bool { return len(sub.all()) == 2 })
	time.Sleep(50 * time.Millisecond)
	must.Eq(t, []string{"a", "b"}, sub.keys())
	must.Eq(t, "norca.r1.Job", sub.all()[0].Subject)
	must.Eq(t, float64(1), m.Value(MetricSkipped, "reason", "own"))
	must.Eq(t, float64(1), m.Value(MetricSkipped, "reason", "type"))
	must.Eq(t, streamCall{Index: 1, Exact: false}, src.Calls()[0])
}

// discardCheckpoint never stores anything, like a bridge that crashes
// before every checkpoint write.
type discardCheckpoint struct{}

func (discardCheckpoint) Load(context.Context) (uint64, bool, error) { return 0, false, nil }
func (discardCheckpoint) Save(context.Context, uint64) error         { return nil }

func TestBridge_JetStreamDeduplication(t *testing.T) {
	s := runNATS(t, true)
	nc := connect(t, s)
	js, err := jetstream.New(nc)
	must.NoError(t, err)
	ctx := context.Background()
	must.NoError(t, EnsureStream(ctx, js, "NOMAD", "nomad", "r1", time.Minute))
	// Existing streams are left alone.
	must.NoError(t, EnsureStream(ctx, js, "NOMAD", "nomad", "r1", time.Hour))
	pub := &JetStreamPublisher{JS: js, Timeout: 5 * time.Second, Stream: "NOMAD"}

	src := newFakeSource()
	src.add("Job", "a", "JobRegistered")
	src.add("Job", "b", "JobRegistered", "JobDeregistered")
	src.add("Node", "n", "NodeRegistration")

	opts := fastOptions("r1")
	opts.StartAt = StartOldest
	for run := 1; run <= 2; run++ {
		b, m := newTestBridge(t, src, pub, discardCheckpoint{}, opts)
		stop := runBridge(b)
		waitFor(t, "published", func() bool { return m.Value(MetricPublishedIndex) == 102 })
		stop()
		if run == 2 {
			// Everything was delivered again and discarded by the stream.
			must.Eq(t, float64(4), m.Value(MetricDuplicates))
		}
	}

	stream, err := js.Stream(ctx, "NOMAD")
	must.NoError(t, err)
	info, err := stream.Info(ctx)
	must.NoError(t, err)
	must.Eq(t, uint64(4), info.State.Msgs)
	must.Eq(t, time.Minute, info.Config.Duplicates)

	msg, err := stream.GetMsg(ctx, 2)
	must.NoError(t, err)
	must.Eq(t, "nomad.r1.Job", msg.Subject)
	must.Eq(t, "r1:101:Job:JobRegistered:default:b", msg.Header.Get(HeaderMsgID))

	// Without a stream for the subjects the publish fails and is retried.
	pub.Stream = "OTHER"
	src.add("Job", "c", "JobRegistered")
	b, m := newTestBridge(t, src, pub, &MemoryCheckpoint{}, fastOptions("r1"))
	stop := runBridge(b)
	waitFor(t, "failures", func() bool { return m.Value(MetricPublishFailures) >= 2 })
	stop()
	must.Eq(t, float64(0), m.Value(MetricPublished, "topic", "Job"))
}

func TestBridge_GoneGapMarker(t *testing.T) {
	s := runNATS(t, false)
	sub := collect(t, connect(t, s), "nomad.r1.>")

	src := newFakeSource()
	for i := range 5 {
		src.add("Job", fmt.Sprintf("old%d", i), "JobRegistered")
	}
	cp := &MemoryCheckpoint{}
	must.NoError(t, cp.Save(context.Background(), 101))
	src.truncate(104) // 102 and 103 were dropped

	b, m := newTestBridge(t, src, &CorePublisher{Conn: connect(t, s)}, cp, fastOptions("r1"))
	stop := runBridge(b)
	defer stop()
	waitFor(t, "gap marker and newest batch", func() bool { return len(sub.all()) == 2 })

	gap := sub.all()[0]
	must.Eq(t, "nomad.r1.Bridge", gap.Subject)
	must.Eq(t, GapType, gap.Header.Get(HeaderType))
	must.Eq(t, "r1:gap:102", gap.Header.Get(HeaderMsgID))
	var ev struct {
		Region, Topic, Type string
		Payload             struct{ Gap GapPayload }
	}
	must.NoError(t, json.Unmarshal(gap.Data, &ev))
	must.Eq(t, "r1", ev.Region)
	must.Eq(t, GapTopic, ev.Topic)
	must.Eq(t, uint64(102), ev.Payload.Gap.FromIndex)
	must.Eq(t, "index-gone", ev.Payload.Gap.Reason)

	// Continued from the newest batch, then exactly.
	must.Eq(t, "old4", sub.all()[1].Header.Get(HeaderKey))
	src.add("Job", "new", "JobRegistered")
	waitFor(t, "new event", func() bool { return len(sub.all()) == 3 })
	must.Eq(t, float64(1), m.Value(MetricGaps))
	calls := src.Calls()
	must.Eq(t, []streamCall{{102, true}, {0, false}}, calls[:2])
	waitFor(t, "checkpoint", func() bool {
		i, _, _ := cp.Load(context.Background())
		return i == 105
	})
}

func TestBridge_PublishFailuresAreRetried(t *testing.T) {
	s := runNATS(t, false)
	sub := collect(t, connect(t, s), "nomad.r1.>")
	src := newFakeSource()
	src.add("Job", "a", "JobRegistered")

	pub := &flakyPublisher{Publisher: &CorePublisher{Conn: connect(t, s)}}
	b, m := newTestBridge(t, src, pub, &MemoryCheckpoint{}, fastOptions("r1"))
	stop := runBridge(b)
	defer stop()
	waitFor(t, "first", func() bool { return len(sub.all()) == 1 })

	pub.failNext(3)
	src.add("Job", "b", "JobRegistered")
	src.add("Job", "c", "JobRegistered")
	waitFor(t, "after failures", func() bool { return len(sub.all()) == 3 })
	must.Eq(t, []string{"a", "b", "c"}, sub.keys())
	must.Eq(t, float64(3), m.Value(MetricPublishFailures))

	// A failing subscription is retried too.
	src.setFail(fmt.Errorf("connection refused"))
	src.drop()
	waitFor(t, "stream errors", func() bool { return m.Value(MetricStreamErrors, "reason", "subscribe") >= 2 })
	src.setFail(nil)
	src.add("Job", "d", "JobRegistered")
	waitFor(t, "recovered", func() bool { return len(sub.all()) == 4 })
}

func TestBridge_CheckpointWrites(t *testing.T) {
	s := runNATS(t, false)
	src := newFakeSource()
	vars := fake.New()
	cp := &VariableCheckpoint{Vars: vars, Namespace: "default", Path: "nomad/jobs/bridge/checkpoint", Region: "r1", Writer: "test"}

	opts := fastOptions("r1")
	opts.CheckpointIdleInterval = time.Hour
	opts.Skip = SkipVariables("default", cp.Path)
	b, _ := newTestBridge(t, src, &CorePublisher{Conn: connect(t, s)}, cp, opts)
	stop := runBridge(b)
	defer stop()

	src.add("Job", "a", "JobRegistered")
	waitFor(t, "checkpoint", func() bool {
		v := vars.GetVariable("default", cp.Path)
		return v != nil && v.Items["index"] == "100"
	})
	must.Eq(t, "r1", vars.GetVariable("default", cp.Path).Items["region"])

	// Only skipped events (the bridge's own writes) do not trigger more
	// writes before the idle interval.
	writes := vars.Calls("WriteVariable")
	src.add("Variables", cp.Path, "VariableUpserted")
	src.add("Variables", cp.Path, "VariableUpserted")
	time.Sleep(200 * time.Millisecond)
	must.Eq(t, writes, vars.Calls("WriteVariable"))

	// A published event does.
	src.add("Job", "b", "JobRegistered")
	waitFor(t, "checkpoint after published event", func() bool {
		return vars.GetVariable("default", cp.Path).Items["index"] == "103"
	})
}

func TestVariableCheckpoint_NeverMovesBackwards(t *testing.T) {
	ctx := context.Background()
	vars := fake.New()
	a := &VariableCheckpoint{Vars: vars, Namespace: "default", Path: "cp", Region: "r", Writer: "a"}
	b := &VariableCheckpoint{Vars: vars, Namespace: "default", Path: "cp", Region: "r", Writer: "b"}

	_, found, err := a.Load(ctx)
	must.NoError(t, err)
	must.False(t, found)
	must.NoError(t, a.Save(ctx, 10))
	must.NoError(t, a.Save(ctx, 12))

	// b has a stale modify index: a lower index is not written, a higher
	// one is, after re-reading.
	must.NoError(t, b.Save(ctx, 11))
	index, _, _ := b.Load(ctx)
	must.Eq(t, uint64(12), index)
	must.NoError(t, a.Save(ctx, 13))
	must.NoError(t, b.Save(ctx, 20))
	index, _, _ = a.Load(ctx)
	must.Eq(t, uint64(20), index)
	must.Eq(t, "b", vars.GetVariable("default", "cp").Items["writer"])

	f := &FileCheckpoint{Path: filepath.Join(t.TempDir(), "cp.json")}
	must.NoError(t, f.Save(ctx, 5))
	must.NoError(t, f.Save(ctx, 3))
	index, found, err = f.Load(ctx)
	must.NoError(t, err)
	must.True(t, found)
	must.Eq(t, uint64(5), index)
}

func TestBridge_LeaderElection(t *testing.T) {
	s := runNATS(t, false)
	sub := collect(t, connect(t, s), "nomad.r1.>")
	src := newFakeSource()
	vars := fake.New()

	type instance struct {
		le      *controller.LeaderElector
		metrics *Metrics
		cancel  context.CancelFunc
		done    chan struct{}
	}
	start := func(id string) *instance {
		le, err := controller.NewLeaderElector(vars, controller.LeaderElectionConfig{
			Namespace: "default", Path: "nomad/jobs/bridge/lease", Identity: id,
			LeaseDuration: 600 * time.Millisecond, RenewDeadline: 400 * time.Millisecond, RetryPeriod: 100 * time.Millisecond,
			ReleaseOnCancel: true,
		}, testLogger(t))
		must.NoError(t, err)
		cp := &VariableCheckpoint{Vars: vars, Namespace: "default", Path: "nomad/jobs/bridge/checkpoint", Region: "r1", Writer: id}
		opts := fastOptions("r1")
		opts.Skip = SkipVariables("default", cp.Path, "nomad/jobs/bridge/lease")
		b, m := newTestBridge(t, src, &CorePublisher{Conn: connect(t, s)}, cp, opts)
		ctx, cancel := context.WithCancel(context.Background())
		in := &instance{le: le, metrics: m, cancel: cancel, done: make(chan struct{})}
		go func() {
			defer close(in.done)
			b.RunElected(ctx, le)
		}()
		return in
	}
	stopInstance := func(in *instance) {
		in.cancel()
		<-in.done
	}

	a := start("a")
	waitFor(t, "a leads", a.le.IsLeader)
	bInst := start("b")
	defer stopInstance(bInst)

	for i := range 3 {
		src.add("Job", fmt.Sprintf("k%d", i), "JobRegistered")
	}
	waitFor(t, "published by a", func() bool { return len(sub.all()) == 3 })
	must.False(t, bInst.le.IsLeader())
	must.Eq(t, float64(1), a.metrics.Value(MetricLeader))
	must.Eq(t, float64(0), bInst.metrics.Value(MetricPublished, "topic", "Job"))

	// a stops: it stores its checkpoint and releases the lease; b takes
	// over and continues after the checkpoint.
	stopInstance(a)
	waitFor(t, "b leads", bInst.le.IsLeader)
	for i := 3; i < 6; i++ {
		src.add("Job", fmt.Sprintf("k%d", i), "JobRegistered")
	}
	waitFor(t, "published by b", func() bool { return len(sub.all()) == 6 })
	time.Sleep(100 * time.Millisecond)
	must.Eq(t, []string{"k0", "k1", "k2", "k3", "k4", "k5"}, sub.keys())
	must.Eq(t, float64(3), bInst.metrics.Value(MetricPublished, "topic", "Job"))
}

func TestMetrics_Text(t *testing.T) {
	m := NewMetrics()
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	m.received(10)
	m.published("Job", 2, 1)
	now = now.Add(3 * time.Second)
	var buf bytes.Buffer
	_, err := m.WriteTo(&buf)
	must.NoError(t, err)
	out := buf.String()
	must.StrContains(t, out, `norca_natsbridge_events_published_total{topic="Job"} 2`)
	must.StrContains(t, out, "norca_natsbridge_events_duplicate_total 1")
	must.StrContains(t, out, "norca_natsbridge_lag_seconds 3")
	must.StrContains(t, out, "norca_natsbridge_lag_index 10")
	m.publishedIndex(10)
	buf.Reset()
	_, _ = m.WriteTo(&buf)
	must.StrContains(t, buf.String(), "norca_natsbridge_lag_seconds 0")
	must.StrContains(t, buf.String(), "norca_natsbridge_lag_index 0")
}

func TestParseConfig(t *testing.T) {
	env := map[string]string{
		"NATS_URL":                  "nats://nats:4222",
		"NATSBRIDGE_JETSTREAM":      "true",
		"NATSBRIDGE_TOPICS":         "Job,Deployment:web",
		"NOMAD_JOB_ID":              "natsbridge",
		"NOMAD_NAMESPACE":           "ops",
		"NATSBRIDGE_SUBJECT_PREFIX": "norca",
	}
	var out bytes.Buffer
	cfg, err := ParseConfig([]string{"-jetstream-stream", "NORCA", "-event-types", "JobRegistered, DeploymentStatusUpdate"},
		func(k string) string { return env[k] }, &out)
	must.NoError(t, err)
	must.Eq(t, "nats://nats:4222", cfg.NATSURL)
	must.True(t, cfg.JetStream)
	must.Eq(t, "NORCA", cfg.JetStreamStream)
	must.Eq(t, "norca", cfg.SubjectPrefix)
	must.Eq(t, "nomad/jobs/natsbridge", cfg.StatePrefix)
	must.Eq(t, "ops", cfg.StateNamespace)
	must.Eq(t, "*", cfg.Namespace)
	must.Eq(t, []string{"JobRegistered", "DeploymentStatusUpdate"}, cfg.EventTypes)
	must.Eq(t, []TopicFilter{{Topic: "Job"}, {Topic: "Deployment", Keys: []string{"web"}}}, cfg.Topics)
	must.True(t, cfg.LeaderElect)
	must.Eq(t, CheckpointVariable, cfg.Checkpoint)

	noEnv := func(string) string { return "" }
	for _, args := range [][]string{
		{"-checkpoint", "file"},
		{"-jetstream-create"},
		{"-jetstream-stream", "X"},
		{"-subject-prefix", "a..b"},
		{"-start-at", "yesterday"},
		{"-nats-cert", "c.pem"},
		{"extra"},
	} {
		_, err := ParseConfig(args, noEnv, &out)
		must.Error(t, err, must.Sprint(args))
	}
	_, err = ParseConfig(nil, func(k string) string {
		if k == "NATSBRIDGE_JETSTREAM" {
			return "maybe"
		}
		return ""
	}, &out)
	must.Error(t, err)
}

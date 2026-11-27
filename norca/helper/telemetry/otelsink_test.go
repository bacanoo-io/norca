// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	metrics "github.com/hashicorp/go-metrics"
	"github.com/shoenig/test/must"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// fakeClock is a clock the tests move by hand.
type fakeClock struct{ ns atomic.Int64 }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.ns.Store(time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC).UnixNano())
	return c
}

func (c *fakeClock) now() time.Time           { return time.Unix(0, c.ns.Load()) }
func (c *fakeClock) advance(d time.Duration)  { c.ns.Add(int64(d)) }
func (c *fakeClock) options() OTelSinkOptions { return OTelSinkOptions{now: c.now} }

// sinkHarness is a bound OTel sink with a manual reader.
type sinkHarness struct {
	t      *testing.T
	reader *sdkmetric.ManualReader
	mp     *sdkmetric.MeterProvider
	sink   *OTelSink
}

func newSinkHarness(t *testing.T, opts OTelSinkOptions, mpOpts ...sdkmetric.Option) *sinkHarness {
	t.Helper()
	return newSinkHarnessReader(t, sdkmetric.NewManualReader(), opts, mpOpts...)
}

func newSinkHarnessReader(t *testing.T, reader *sdkmetric.ManualReader, opts OTelSinkOptions, mpOpts ...sdkmetric.Option) *sinkHarness {
	t.Helper()
	mp := sdkmetric.NewMeterProvider(append(mpOpts, sdkmetric.WithReader(reader))...)
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	return &sinkHarness{t: t, reader: reader, mp: mp, sink: NewOTelSink(mp, opts)}
}

// collect returns the metrics of the sink's scope by name.
func (h *sinkHarness) collect() map[string]metricdata.Metrics {
	h.t.Helper()
	var rm metricdata.ResourceMetrics
	must.NoError(h.t, h.reader.Collect(context.Background(), &rm))
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		must.Eq(h.t, meterName, sm.Scope.Name)
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func attrsOf(set attribute.Set) map[string]string {
	out := map[string]string{}
	for _, kv := range set.ToSlice() {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

// points returns the value of each data point of a gauge or sum by its
// attributes, formatted as k=v,k=v.
func points(t *testing.T, m metricdata.Metrics) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	add := func(set attribute.Set, v float64) {
		out[set.Encoded(attribute.DefaultEncoder())] = v
	}
	switch data := m.Data.(type) {
	case metricdata.Gauge[float64]:
		for _, p := range data.DataPoints {
			add(p.Attributes, p.Value)
		}
	case metricdata.Sum[float64]:
		must.True(t, data.IsMonotonic, must.Sprint("a counter is monotonic"))
		for _, p := range data.DataPoints {
			add(p.Attributes, p.Value)
		}
	default:
		t.Fatalf("%s: unexpected data %T", m.Name, m.Data)
	}
	return out
}

// Every go-metrics call kind reaches the SDK as its instrument, named by the
// key with the service name, with the labels but without the host label.
func TestOTelSink_CallKinds(t *testing.T) {
	ci.Parallel(t)
	h := newSinkHarness(t, OTelSinkOptions{})

	conf := metrics.DefaultConfig(MetricsServiceName)
	conf.HostName = "node-1"
	conf.EnableHostnameLabel = true
	conf.EnableRuntimeMetrics = false
	m, err := metrics.New(conf, h.sink)
	must.NoError(t, err)

	alloc := []metrics.Label{{Name: "alloc_id", Value: "a1"}, {Name: "task", Value: "web"}}
	m.SetGauge([]string{"client", "uptime"}, 12)
	m.SetGaugeWithLabels([]string{"client", "allocs", "memory", "usage"}, 100, alloc)
	m.SetGaugeWithLabels([]string{"client", "allocs", "memory", "usage"}, 150, alloc)
	m.SetPrecisionGauge([]string{"server", "broker", "total_ready"}, 1e12+1)
	m.IncrCounter([]string{"server", "rpc", "request"}, 1)
	m.IncrCounter([]string{"server", "rpc", "request"}, 2)
	m.IncrCounterWithLabels([]string{"server", "rpc", "job", "write"}, 1, []metrics.Label{{Name: "identity", Value: "token"}})
	m.AddSample([]string{"raft", "commitTime"}, 3)
	m.AddSampleWithLabels([]string{"server", "fsm", "apply_sv_operation"}, 7, []metrics.Label{{Name: "op", Value: "set"}})
	m.MeasureSince([]string{"server", "rpc", "request_latency"}, time.Now().Add(-25*time.Millisecond))
	m.EmitKey([]string{"server", "emitted"}, 1)

	got := h.collect()
	must.MapNotContainsKey(t, got, "norca.server.emitted")
	must.MapNotContainsKey(t, got, "norca.node-1.client.uptime", must.Sprint("the hostname is a label, not a key part"))

	must.Eq(t, map[string]float64{"": 12}, points(t, got["norca.client.uptime"]))
	must.Eq(t, map[string]float64{"alloc_id=a1,task=web": 150}, points(t, got["norca.client.allocs.memory.usage"]))
	must.Eq(t, map[string]float64{"": 1e12 + 1}, points(t, got["norca.server.broker.total_ready"]))
	must.Eq(t, map[string]float64{"": 3}, points(t, got["norca.server.rpc.request"]))
	must.Eq(t, map[string]float64{"identity=token": 1}, points(t, got["norca.server.rpc.job.write"]))

	for name, want := range map[string]struct {
		attrs map[string]string
		min   float64
	}{
		"norca.raft.commitTime":               {attrs: map[string]string{}, min: 3},
		"norca.server.fsm.apply_sv_operation": {attrs: map[string]string{"op": "set"}, min: 7},
		"norca.server.rpc.request_latency":    {attrs: map[string]string{}, min: 25},
	} {
		m := got[name]
		must.Eq(t, "ms", m.Unit, must.Sprint(name))
		hist, ok := m.Data.(metricdata.Histogram[float64])
		must.True(t, ok, must.Sprintf("%s is a histogram, not %T", name, m.Data))
		must.Len(t, 1, hist.DataPoints)
		p := hist.DataPoints[0]
		must.Eq(t, want.attrs, attrsOf(p.Attributes), must.Sprint(name))
		must.Eq(t, uint64(1), p.Count)
		must.GreaterEq(t, want.min, p.Sum, must.Sprint(name))
		// the SDK's explicit default buckets, in milliseconds
		must.Eq(t, []float64{0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 2500, 5000, 7500, 10000}, p.Bounds)
	}

	// No series carries the host label.
	for name, m := range got {
		switch data := m.Data.(type) {
		case metricdata.Gauge[float64]:
			for _, p := range data.DataPoints {
				_, ok := p.Attributes.Value(hostLabel)
				must.False(t, ok, must.Sprint(name))
			}
		case metricdata.Sum[float64]:
			for _, p := range data.DataPoints {
				_, ok := p.Attributes.Value(hostLabel)
				must.False(t, ok, must.Sprint(name))
			}
		}
	}
}

// The prefix filters of go-metrics apply before the sink.
func TestOTelSink_PrefixFilter(t *testing.T) {
	ci.Parallel(t)
	h := newSinkHarness(t, OTelSinkOptions{})

	conf := metrics.DefaultConfig(MetricsServiceName)
	conf.EnableHostname = false
	conf.EnableRuntimeMetrics = false
	conf.BlockedPrefixes = []string{"norca.raft.replication"}
	conf.AllowedPrefixes = []string{"norca.raft.replication.appendEntries.rpc"}
	m, err := metrics.New(conf, h.sink)
	must.NoError(t, err)

	m.IncrCounter([]string{"raft", "replication", "heartbeat"}, 1)
	m.AddSample([]string{"raft", "replication", "appendEntries", "rpc"}, 2)
	m.IncrCounter([]string{"raft", "apply"}, 1)

	got := h.collect()
	must.MapNotContainsKey(t, got, "norca.raft.replication.heartbeat")
	must.MapContainsKey(t, got, "norca.raft.replication.appendEntries.rpc")
	must.MapContainsKey(t, got, "norca.raft.apply")
}

// A gauge or counter series not updated within the expiry is no longer
// reported; an update brings it back, a counter from zero.
func TestOTelSink_Expiry(t *testing.T) {
	ci.Parallel(t)
	clock := newFakeClock()
	opts := clock.options()
	opts.Expiry = time.Minute
	h := newSinkHarness(t, opts)

	gone := []metrics.Label{{Name: "alloc_id", Value: "gone"}}
	live := []metrics.Label{{Name: "alloc_id", Value: "live"}}
	gauge := []string{"norca", "client", "allocs", "cpu", "total_percent"}
	counter := []string{"norca", "client", "allocs", "network_policy", "dropped"}
	h.sink.SetGaugeWithLabels(gauge, 1, gone)
	h.sink.SetGaugeWithLabels(gauge, 2, live)
	h.sink.IncrCounterWithLabels(counter, 5, gone)
	h.sink.IncrCounterWithLabels(counter, 7, live)

	clock.advance(40 * time.Second)
	h.sink.SetGaugeWithLabels(gauge, 3, live)
	h.sink.IncrCounterWithLabels(counter, 1, live)
	got := h.collect()
	must.Eq(t, map[string]float64{"alloc_id=gone": 1, "alloc_id=live": 3}, points(t, got["norca.client.allocs.cpu.total_percent"]))
	must.Eq(t, map[string]float64{"alloc_id=gone": 5, "alloc_id=live": 8}, points(t, got["norca.client.allocs.network_policy.dropped"]))

	clock.advance(30 * time.Second) // gone: 70s old, live: 30s
	got = h.collect()
	must.Eq(t, map[string]float64{"alloc_id=live": 3}, points(t, got["norca.client.allocs.cpu.total_percent"]))
	must.Eq(t, map[string]float64{"alloc_id=live": 8}, points(t, got["norca.client.allocs.network_policy.dropped"]))

	clock.advance(time.Minute) // every series expired
	got = h.collect()
	must.MapNotContainsKey(t, got, "norca.client.allocs.cpu.total_percent")
	must.MapNotContainsKey(t, got, "norca.client.allocs.network_policy.dropped")

	h.sink.IncrCounterWithLabels(counter, 2, gone)
	got = h.collect()
	must.Eq(t, map[string]float64{"alloc_id=gone": 2}, points(t, got["norca.client.allocs.network_policy.dropped"]),
		must.Sprint("an expired counter starts again from zero"))
}

// Beyond the cardinality limit of the provider the SDK folds the series
// into its overflow series.
func TestOTelSink_CardinalityOverflow(t *testing.T) {
	ci.Parallel(t)
	h := newSinkHarness(t, OTelSinkOptions{}, sdkmetric.WithCardinalityLimit(3))

	key := []string{"norca", "client", "allocs", "memory", "rss"}
	for i := range 5 {
		h.sink.SetGaugeWithLabels(key, float32(i), []metrics.Label{{Name: "alloc_id", Value: fmt.Sprint(i)}})
	}
	got := points(t, h.collect()["norca.client.allocs.memory.rss"])
	must.MapLen(t, 3, got)
	must.MapContainsKey(t, got, "otel.metric.overflow=true")
}

// A sink created without a provider keeps its gauges and counters until it
// is bound and drops the samples; only the first Bind counts.
func TestOTelSink_Bind(t *testing.T) {
	ci.Parallel(t)
	sink := NewOTelSink(nil, OTelSinkOptions{})
	sink.SetGauge([]string{"norca", "runtime", "num_goroutines"}, 42)
	sink.IncrCounter([]string{"norca", "server", "rpc", "request"}, 3)
	sink.AddSample([]string{"norca", "raft", "apply"}, 1)

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	sink.Bind(mp)
	other := sdkmetric.NewManualReader()
	otherMP := sdkmetric.NewMeterProvider(sdkmetric.WithReader(other))
	t.Cleanup(func() { _ = otherMP.Shutdown(context.Background()) })
	sink.Bind(otherMP)

	sink.IncrCounter([]string{"norca", "server", "rpc", "request"}, 1)
	sink.AddSample([]string{"norca", "raft", "apply"}, 2)

	h := &sinkHarness{t: t, reader: reader, mp: mp, sink: sink}
	got := h.collect()
	must.Eq(t, map[string]float64{"": 42}, points(t, got["norca.runtime.num_goroutines"]))
	must.Eq(t, map[string]float64{"": 4}, points(t, got["norca.server.rpc.request"]))
	hist := got["norca.raft.apply"].Data.(metricdata.Histogram[float64])
	must.Eq(t, uint64(1), hist.DataPoints[0].Count, must.Sprint("the sample before Bind is dropped"))
	must.Eq(t, 2.0, hist.DataPoints[0].Sum)

	var rm metricdata.ResourceMetrics
	must.NoError(t, other.Collect(context.Background(), &rm))
	must.SliceEmpty(t, rm.ScopeMetrics, must.Sprint("a second Bind is ignored"))
}

// A key that is not a valid instrument name is dropped and logged once.
func TestOTelSink_InvalidName(t *testing.T) {
	ci.Parallel(t)
	var out syncBuffer
	opts := OTelSinkOptions{Logger: testLogger(&out)}
	h := newSinkHarness(t, opts)

	bad := []string{"norca", "raft", "replication", "appendEntries", "rpc", "10.0.0.1:4647"}
	for range 3 {
		h.sink.AddSample(bad, 1)
		h.sink.SetGauge(bad, 1)
	}
	h.sink.SetGauge([]string{"norca", "raft", "lastIndex"}, 9)

	got := h.collect()
	must.MapLen(t, 1, got)
	must.MapContainsKey(t, got, "norca.raft.lastIndex")
	must.Len(t, 1, out.lines("invalid instrument name"))
}

func TestValidInstrumentName(t *testing.T) {
	ci.Parallel(t)
	cases := map[string]bool{
		"norca.server.rpc.request":         true,
		"norca.client.allocs.cpu.user":     true,
		"norca.raft.replication.rpc.a-b_c": true,
		"a":                                true,
		"norca/x":                          true,
		"":                                 false,
		"1norca":                           false,
		".norca":                           false,
		"norca.raft.10.0.0.1:4647":         false,
		"norca.job.my job":                 false,
		"norca.ü":                          false,
	}
	long := "n"
	for len(long) < 255 {
		long += "x"
	}
	cases[long] = true
	cases[long+"x"] = false
	for name, want := range cases {
		must.Eq(t, want, validInstrumentName(name), must.Sprint(name))
	}
}

// The samples of the libraries that are not times get their own unit and
// buckets.
func TestOTelSink_SampleUnits(t *testing.T) {
	ci.Parallel(t)
	h := newSinkHarness(t, OTelSinkOptions{})
	h.sink.AddSample([]string{"norca", "runtime", "gc_pause_ns"}, 120_000)
	h.sink.AddSample([]string{"norca", "serf", "queue", "Intent"}, 3)
	h.sink.AddSample([]string{"norca", "memberlist", "size", "remote"}, 900)
	h.sink.AddSample([]string{"norca", "raft", "leader", "lastContact"}, 12)

	got := h.collect()
	for name, want := range map[string]sampleSpec{
		"norca.runtime.gc_pause_ns":     nanosSpec,
		"norca.serf.queue.Intent":       countSpec,
		"norca.memberlist.size.remote":  bytesSpec,
		"norca.raft.leader.lastContact": millisSpec,
	} {
		m := got[name]
		must.Eq(t, want.unit, m.Unit, must.Sprint(name))
		bounds := m.Data.(metricdata.Histogram[float64]).DataPoints[0].Bounds
		if want.bounds != nil {
			must.Eq(t, want.bounds, bounds, must.Sprint(name))
		} else {
			must.Eq(t, 10000.0, bounds[len(bounds)-1], must.Sprint(name))
		}
	}
	for name := range sampleSpecs {
		must.StrHasPrefix(t, MetricsServiceName+".", name)
		must.True(t, validInstrumentName(name), must.Sprint(name))
	}
}

// With the exponential aggregation of metrics.histograms the samples are
// exponential histograms.
func TestOTelSink_ExponentialHistograms(t *testing.T) {
	ci.Parallel(t)
	reader := sdkmetric.NewManualReader(sdkmetric.WithAggregationSelector(aggregationSelector(HistogramsExponential)))
	h := newSinkHarnessReader(t, reader, OTelSinkOptions{})
	h.sink.AddSample([]string{"norca", "raft", "commitTime"}, 3)
	_, ok := h.collect()["norca.raft.commitTime"].Data.(metricdata.ExponentialHistogram[float64])
	must.True(t, ok)
}

// Updates, new series and collections run concurrently (go test -race).
func TestOTelSink_Concurrent(t *testing.T) {
	ci.Parallel(t)
	h := newSinkHarness(t, OTelSinkOptions{})
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 500 {
				labels := []metrics.Label{{Name: "alloc_id", Value: fmt.Sprint(i % 20)}}
				h.sink.SetGaugeWithLabels([]string{"norca", "client", "allocs", "cpu", "user"}, float32(w), labels)
				h.sink.IncrCounterWithLabels([]string{"norca", "server", "rpc", "request"}, 1, nil)
				h.sink.AddSampleWithLabels([]string{"norca", "server", fmt.Sprint("x", i%5)}, 1, labels)
			}
		})
	}
	stop := make(chan struct{})
	collected := make(chan struct{})
	go func() {
		defer close(collected)
		for {
			select {
			case <-stop:
				return
			default:
				var rm metricdata.ResourceMetrics
				_ = h.reader.Collect(context.Background(), &rm)
			}
		}
	}()
	wg.Wait()
	close(stop)
	<-collected
	must.Eq(t, map[string]float64{"": 8 * 500}, points(t, h.collect()["norca.server.rpc.request"]))
}

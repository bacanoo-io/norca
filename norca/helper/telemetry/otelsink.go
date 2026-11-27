// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"
	metrics "github.com/hashicorp/go-metrics"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// DefaultSeriesExpiry is how long the OTel sink reports a gauge or counter
// series after its last update, as the Prometheus sink does by default.
const DefaultSeriesExpiry = 60 * time.Second

// meterName is the instrumentation scope of the bridged metrics.
const meterName = "github.com/bacanoo-io/norca/norca/helper/telemetry"

// hostLabel is the label go-metrics adds with the hostname; host.name is a
// resource attribute of the export instead.
const hostLabel = "host"

// Bytes that separate the parts of a series key; neither occurs in UTF-8.
const (
	labelSep = 0xff
	valueSep = 0xfe
)

// OTelSinkOptions are the options of NewOTelSink.
type OTelSinkOptions struct {
	// Expiry is how long a gauge or counter series is reported after its
	// last update (default DefaultSeriesExpiry), so the series of a stopped
	// allocation disappear.
	Expiry time.Duration

	// Logger logs each invalid instrument name once (default: no logging).
	Logger hclog.Logger

	// now is the clock; tests replace it.
	now func() time.Time
}

// OTelSink is a go-metrics sink that bridges the agent's metrics to the
// OpenTelemetry metric SDK, so every go-metrics call site (and Raft, Serf
// and memberlist, which emit through the same global) reaches the OTLP
// export without a change. It goes into the go-metrics fanout next to the
// in-memory and Prometheus sinks, which see the same names.
//
// The mapping:
//
//   - A gauge is a Float64ObservableGauge: the sink keeps the last value per
//     series and reports it while it is younger than the expiry.
//   - A counter is a Float64ObservableCounter: the sink keeps the running
//     sum per series and reports it while it is younger than the expiry.
//     The observable form keeps the series bounded: the SDK forgets a
//     series the sink no longer reports, while a synchronous counter would
//     keep every attribute set of every allocation forever.
//   - A sample (MeasureSince, AddSample) is a Float64Histogram with the unit
//     ms, the unit of the go-metrics timers. The buckets are the SDK's
//     explicit default (0 to 10 000, which suits milliseconds) unless the
//     reader selects the exponential histogram. The few samples of the
//     libraries that are not times (sizes, counts, ratios, the Go runtime's
//     GC pauses in nanoseconds) get their own unit and buckets from
//     sampleSpecs.
//   - EmitKey is ignored.
//
// The instrument name is the go-metrics key joined with dots, service name
// included (norca.server.rpc.request); the labels are the attributes,
// without the host label. Instruments are created on first use; a name
// that is not a valid OpenTelemetry instrument name is logged once and
// dropped. The prefix filters of go-metrics apply before any sink.
//
// The sink can exist before its meter provider: it is created with the
// fanout and bound once the providers start, just before the agent. Until
// then it keeps the gauge and counter series, so the counts since the
// telemetry was set up are not lost, and drops the samples.
type OTelSink struct {
	logger hclog.Logger
	expiry time.Duration
	now    func() time.Time

	// meter is nil until Bind.
	meter atomic.Pointer[metric.Meter]

	gauges   seriesTable
	counters seriesTable
	samples  seriesTable

	invalidMu sync.Mutex
	invalid   map[string]struct{}
}

var (
	_ metrics.MetricSink               = (*OTelSink)(nil)
	_ metrics.PrecisionGaugeMetricSink = (*OTelSink)(nil)
)

// NewOTelSink returns a sink that records into the meter provider mp, or
// into nothing until Bind when mp is nil.
func NewOTelSink(mp metric.MeterProvider, opts OTelSinkOptions) *OTelSink {
	s := &OTelSink{
		logger:  opts.Logger,
		expiry:  opts.Expiry,
		now:     opts.now,
		invalid: map[string]struct{}{},
	}
	if s.logger == nil {
		s.logger = hclog.NewNullLogger()
	}
	if s.expiry <= 0 {
		s.expiry = DefaultSeriesExpiry
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.gauges.init(kindGauge)
	s.counters.init(kindCounter)
	s.samples.init(kindHistogram)
	s.Bind(mp)
	return s
}

// Bind attaches the sink to its meter provider and creates the instruments
// of the series kept so far. Only the first call with a provider counts.
func (s *OTelSink) Bind(mp metric.MeterProvider) {
	if mp == nil {
		return
	}
	m := mp.Meter(meterName)
	if !s.meter.CompareAndSwap(nil, &m) {
		return
	}
	for _, t := range []*seriesTable{&s.gauges, &s.counters} {
		for _, inst := range t.snapshot() {
			s.register(inst)
		}
	}
}

// SetGauge implements metrics.MetricSink.
func (s *OTelSink) SetGauge(key []string, val float32) {
	s.SetPrecisionGaugeWithLabels(key, float64(val), nil)
}

// SetGaugeWithLabels implements metrics.MetricSink.
func (s *OTelSink) SetGaugeWithLabels(key []string, val float32, labels []metrics.Label) {
	s.SetPrecisionGaugeWithLabels(key, float64(val), labels)
}

// SetPrecisionGauge implements metrics.PrecisionGaugeMetricSink.
func (s *OTelSink) SetPrecisionGauge(key []string, val float64) {
	s.SetPrecisionGaugeWithLabels(key, val, nil)
}

// SetPrecisionGaugeWithLabels implements metrics.PrecisionGaugeMetricSink.
func (s *OTelSink) SetPrecisionGaugeWithLabels(key []string, val float64, labels []metrics.Label) {
	var buf [256]byte
	ser := s.series(&s.gauges, buf[:0], key, labels)
	if ser == nil {
		return
	}
	ser.value.Store(math.Float64bits(val))
	ser.updated.Store(s.now().UnixNano())
}

// EmitKey implements metrics.MetricSink; key/value pairs are not exported.
func (s *OTelSink) EmitKey([]string, float32) {}

// IncrCounter implements metrics.MetricSink.
func (s *OTelSink) IncrCounter(key []string, val float32) {
	s.IncrCounterWithLabels(key, val, nil)
}

// IncrCounterWithLabels implements metrics.MetricSink.
func (s *OTelSink) IncrCounterWithLabels(key []string, val float32, labels []metrics.Label) {
	var buf [256]byte
	ser := s.series(&s.counters, buf[:0], key, labels)
	if ser == nil {
		return
	}
	for {
		old := ser.value.Load()
		if ser.value.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+float64(val))) {
			break
		}
	}
	ser.updated.Store(s.now().UnixNano())
}

// AddSample implements metrics.MetricSink.
func (s *OTelSink) AddSample(key []string, val float32) {
	s.AddSampleWithLabels(key, val, nil)
}

// AddSampleWithLabels implements metrics.MetricSink.
func (s *OTelSink) AddSampleWithLabels(key []string, val float32, labels []metrics.Label) {
	if s.meter.Load() == nil {
		return
	}
	var buf [256]byte
	ser := s.series(&s.samples, buf[:0], key, labels)
	if ser == nil {
		return
	}
	if h := ser.inst.histogram.Load(); h != nil {
		(*h).Record(context.Background(), float64(val), ser.opt)
	}
}

// series returns the series of key and labels in t, creating it and its
// instrument on first use; nil for an invalid name.
func (s *OTelSink) series(t *seriesTable, buf []byte, key []string, labels []metrics.Label) *series {
	for i, part := range key {
		if i > 0 {
			buf = append(buf, '.')
		}
		buf = append(buf, part...)
	}
	nameLen := len(buf)
	for _, l := range labels {
		if l.Name == hostLabel {
			continue
		}
		buf = append(buf, labelSep)
		buf = append(buf, l.Name...)
		buf = append(buf, valueSep)
		buf = append(buf, l.Value...)
	}

	t.mu.RLock()
	ser := t.series[string(buf)]
	t.mu.RUnlock()
	if ser != nil && !ser.dead.Load() {
		return ser
	}
	return s.newSeries(t, string(buf), nameLen, labels)
}

// newSeries is the slow path of series.
func (s *OTelSink) newSeries(t *seriesTable, key string, nameLen int, labels []metrics.Label) *series {
	name := key[:nameLen]
	if !validInstrumentName(name) {
		s.logInvalid(name)
		return nil
	}
	kvs := make([]attribute.KeyValue, 0, len(labels))
	for _, l := range labels {
		if l.Name != hostLabel {
			kvs = append(kvs, attribute.String(l.Name, l.Value))
		}
	}
	set := attribute.NewSet(kvs...)

	t.mu.Lock()
	if ser := t.series[key]; ser != nil && !ser.dead.Load() {
		t.mu.Unlock()
		return ser
	}
	inst := t.instruments[name]
	if inst == nil {
		inst = &instrument{sink: s, table: t, name: name}
		if t.kind != kindHistogram {
			inst.series = map[string]*series{}
		}
		t.instruments[name] = inst
	}
	ser := &series{inst: inst, opt: metric.WithAttributeSet(set)}
	ser.updated.Store(s.now().UnixNano())
	t.series[key] = ser
	if inst.series != nil {
		inst.series[key] = ser
	}
	t.mu.Unlock()

	// Outside the lock: the SDK holds its own lock while it runs the
	// callbacks, which take the table's lock.
	if s.meter.Load() != nil {
		s.register(inst)
	}
	return ser
}

// register creates the OpenTelemetry instrument of inst once the sink is
// bound.
func (s *OTelSink) register(inst *instrument) {
	m := s.meter.Load()
	if m == nil {
		return
	}
	inst.once.Do(func() {
		var err error
		switch inst.table.kind {
		case kindGauge:
			_, err = (*m).Float64ObservableGauge(inst.name, metric.WithFloat64Callback(inst.observe))
		case kindCounter:
			_, err = (*m).Float64ObservableCounter(inst.name, metric.WithFloat64Callback(inst.observe))
		case kindHistogram:
			spec := sampleSpecFor(inst.name)
			opts := []metric.Float64HistogramOption{metric.WithUnit(spec.unit)}
			if spec.bounds != nil {
				opts = append(opts, metric.WithExplicitBucketBoundaries(spec.bounds...))
			}
			var h metric.Float64Histogram
			h, err = (*m).Float64Histogram(inst.name, opts...)
			if err == nil {
				inst.histogram.Store(&h)
			}
		}
		if err != nil {
			s.logger.Warn("metric not exported over OTLP", "metric", inst.name, "error", err)
		}
	})
}

// sampleSpec is the unit and the explicit buckets of a histogram; nil
// bounds are the SDK's default.
type sampleSpec struct {
	unit   string
	bounds []float64
}

var (
	millisSpec = sampleSpec{unit: "ms"}
	nanosSpec  = sampleSpec{unit: "ns", bounds: []float64{
		0, 5e3, 1e4, 2.5e4, 5e4, 7.5e4, 1e5, 2.5e5, 5e5, 7.5e5, 1e6, 2.5e6, 5e6, 7.5e6, 1e7}}
	bytesSpec = sampleSpec{unit: "By", bounds: []float64{
		0, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216}}
	countSpec = sampleSpec{unit: "1", bounds: []float64{
		0, 1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}}
	ratioSpec = sampleSpec{unit: "1", bounds: []float64{
		0, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1}}
	rateSpec = sampleSpec{unit: "1/s", bounds: []float64{
		0, 10, 100, 1000, 1e4, 1e5, 1e6, 1e7}}
)

// sampleSpecs are the samples that are not times in milliseconds, by
// instrument name: the go-metrics API has one call for every distribution,
// and the Go runtime, Raft, raft-boltdb, memberlist and Serf use it for
// sizes, counts and ratios too. The module's own samples are all times.
var sampleSpecs = map[string]sampleSpec{
	MetricsServiceName + ".runtime.gc_pause_ns":         nanosSpec,
	MetricsServiceName + ".raft.fsm.applyBatchNum":      countSpec,
	MetricsServiceName + ".raft.thread.main.saturation": ratioSpec,
	MetricsServiceName + ".raft.thread.fsm.saturation":  ratioSpec,
	MetricsServiceName + ".raft.boltdb.logSize":         bytesSpec,
	MetricsServiceName + ".raft.boltdb.logsPerBatch":    countSpec,
	MetricsServiceName + ".raft.boltdb.logBatchSize":    bytesSpec,
	MetricsServiceName + ".raft.boltdb.writeCapacity":   rateSpec,
	MetricsServiceName + ".memberlist.queue.broadcasts": countSpec,
	MetricsServiceName + ".memberlist.size.remote":      bytesSpec,
	MetricsServiceName + ".serf.msgs.received":          bytesSpec,
	MetricsServiceName + ".serf.msgs.sent":              bytesSpec,
}

// serfQueuePrefix names the queue depths of Serf (Intent, Event, Query).
const serfQueuePrefix = MetricsServiceName + ".serf.queue."

// sampleSpecFor returns the unit and buckets of a sample.
func sampleSpecFor(name string) sampleSpec {
	if spec, ok := sampleSpecs[name]; ok {
		return spec
	}
	if strings.HasPrefix(name, serfQueuePrefix) {
		return countSpec
	}
	return millisSpec
}

// logInvalid logs an invalid instrument name the first time it is seen.
func (s *OTelSink) logInvalid(name string) {
	s.invalidMu.Lock()
	_, seen := s.invalid[name]
	if !seen {
		s.invalid[name] = struct{}{}
	}
	s.invalidMu.Unlock()
	if !seen {
		s.logger.Warn("metric not exported over OTLP: invalid instrument name", "metric", name)
	}
}

// validInstrumentName reports whether name is a valid OpenTelemetry
// instrument name: a letter, then up to 254 of [A-Za-z0-9_.-/].
func validInstrumentName(name string) bool {
	if name == "" || len(name) > 255 || !isLetter(name[0]) {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if !isLetter(c) && (c < '0' || c > '9') && c != '_' && c != '.' && c != '-' && c != '/' {
			return false
		}
	}
	return true
}

func isLetter(c byte) bool {
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

type instrumentKind uint8

const (
	kindGauge instrumentKind = iota + 1
	kindCounter
	kindHistogram
)

// seriesTable holds the series and instruments of one kind. The gauge and
// counter series expire in the callbacks; the sample series stay for the
// life of the sink, as the SDK keeps the attribute sets of a histogram
// anyway (up to the cardinality limit), and no sample is labelled by
// allocation.
type seriesTable struct {
	kind instrumentKind

	mu          sync.RWMutex
	series      map[string]*series     // by name and labels
	instruments map[string]*instrument // by name
}

func (t *seriesTable) init(kind instrumentKind) {
	t.kind = kind
	t.series = map[string]*series{}
	t.instruments = map[string]*instrument{}
}

func (t *seriesTable) snapshot() []*instrument {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]*instrument, 0, len(t.instruments))
	for _, inst := range t.instruments {
		out = append(out, inst)
	}
	return out
}

// instrument is one OpenTelemetry instrument of the sink.
type instrument struct {
	sink  *OTelSink
	table *seriesTable
	name  string
	once  sync.Once

	// histogram is set once registered (samples only).
	histogram atomic.Pointer[metric.Float64Histogram]

	// series are the series a callback reports (gauges and counters),
	// guarded by table.mu.
	series map[string]*series
}

// observe is the callback of a gauge or counter: it reports the series
// updated within the expiry and forgets the others.
func (inst *instrument) observe(_ context.Context, o metric.Float64Observer) error {
	cutoff := inst.sink.now().Add(-inst.sink.expiry).UnixNano()
	t := inst.table
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, ser := range inst.series {
		if ser.updated.Load() < cutoff {
			ser.dead.Store(true)
			delete(inst.series, key)
			delete(t.series, key)
			continue
		}
		o.Observe(math.Float64frombits(ser.value.Load()), ser.opt)
	}
	return nil
}

// series is one go-metrics series: a name and a label set.
type series struct {
	inst *instrument
	opt  metric.MeasurementOption

	// value holds the float64 bits of the last value (gauge) or the running
	// sum (counter).
	value atomic.Uint64

	// updated is the time of the last update, in Unix nanoseconds.
	updated atomic.Int64

	// dead marks an expired series; an update after the expiry creates a
	// new one.
	dead atomic.Bool
}

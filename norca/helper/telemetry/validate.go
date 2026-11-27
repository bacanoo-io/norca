// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"
)

// Signal names. They are also the values of the signal label of the export
// error counter.
const (
	SignalTraces  = "traces"
	SignalMetrics = "metrics"
	SignalLogs    = "logs"

	// SignalEvents is the event export, which sends through the logs
	// signal's target.
	SignalEvents = "events"
)

// dnsScheme is the only target scheme an endpoint may carry.
const dnsScheme = "dns:///"

// reservedAttributePrefix is the namespace of the attributes the agent sets
// itself; resource_attributes may not use it.
const reservedAttributePrefix = "norca."

// Settings is the validated otlp block, with every default applied.
type Settings struct {
	Compression        string
	Timeout            time.Duration
	Retry              RetrySettings
	ResourceAttributes map[string]string

	Metrics MetricsSettings
	Traces  TracesSettings
	Logs    LogsSettings

	// Warnings are logged when the exporters are created.
	Warnings []string
}

// RetrySettings is the validated retry block.
type RetrySettings struct {
	Enabled         bool
	InitialInterval time.Duration
	MaxInterval     time.Duration
	MaxElapsed      time.Duration
}

// Target is where and how one signal is sent.
type Target struct {
	// Endpoint is the gRPC target as configured.
	Endpoint string

	// Host is the host of the endpoint, the default TLS server name.
	Host string

	// Loopback is true for 127.0.0.0/8, ::1 and localhost.
	Loopback bool

	// Plaintext sends without TLS.
	Plaintext bool

	// TLS is the tls block; nil with TLS means the system roots.
	TLS *TLSConfig

	// Headers are the inline headers, names in lower case.
	Headers map[string]string

	// HeadersFile is read at startup and on reload.
	HeadersFile string
}

// MetricsSettings is the validated metrics block.
type MetricsSettings struct {
	Enabled     bool
	Target      Target
	Interval    time.Duration
	Temporality string
	Histograms  string
}

// TracesSettings is the validated traces block.
type TracesSettings struct {
	Enabled         bool
	Target          Target
	SampleRatio     float64
	TrustHTTPParent bool
	ExcludeRPC      []string
	MaxQueueSize    int
	MaxExportBatch  int
}

// LogsSettings is the validated logs block.
type LogsSettings struct {
	Enabled bool
	Target  Target
	Level   hclog.Level
	Events  EventsSettings
}

// EventsSettings is the validated events block of the logs block.
type EventsSettings struct {
	Enabled bool

	// Topics are the exported topics, without duplicates.
	Topics []string
}

// Resolve validates the otlp block and applies the defaults; it reads no
// file. It returns nil settings for a nil block. Every error names the key
// it is about.
func Resolve(c *OTLPConfig) (*Settings, error) {
	if c == nil {
		return nil, nil
	}
	v := &validator{}
	s := &Settings{
		Compression: DefaultCompression,
		Timeout:     v.duration("timeout", c.Timeout, DefaultTimeout),
		Retry:       v.retry(c.Retry),
	}
	switch c.Compression {
	case "":
	case CompressionGzip, CompressionNone:
		s.Compression = c.Compression
	default:
		v.fail("compression", fmt.Errorf("must be %q or %q, not %q", CompressionGzip, CompressionNone, c.Compression))
	}
	s.ResourceAttributes = v.resourceAttributes(c.ResourceAttributes)

	m := c.Metrics
	if m == nil {
		m = &MetricsConfig{}
	}
	s.Metrics = MetricsSettings{
		Enabled:     boolOr(m.Enabled, true),
		Target:      v.target("metrics", c.TargetConfig.merge(m.TargetConfig), boolOr(m.Enabled, true)),
		Interval:    v.duration("metrics.interval", m.Interval, DefaultMetricsInterval),
		Temporality: v.enum("metrics.temporality", m.Temporality, DefaultTemporality, TemporalityCumulative, TemporalityDelta),
		Histograms:  v.enum("metrics.histograms", m.Histograms, DefaultHistograms, HistogramsExplicit, HistogramsExponential),
	}

	t := c.Traces
	if t == nil {
		t = &TracesConfig{}
	}
	s.Traces = TracesSettings{
		Enabled:         boolOr(t.Enabled, true),
		Target:          v.target("traces", c.TargetConfig.merge(t.TargetConfig), boolOr(t.Enabled, true)),
		SampleRatio:     DefaultSampleRatio,
		TrustHTTPParent: boolOr(t.TrustHTTPParent, false),
		ExcludeRPC:      slices.Clone(t.ExcludeRPC),
		MaxQueueSize:    v.positive("traces.max_queue_size", t.MaxQueueSize, DefaultMaxQueueSize),
		MaxExportBatch:  v.positive("traces.max_export_batch", t.MaxExportBatch, DefaultMaxExportBatch),
	}
	if t.SampleRatio != nil {
		if r := *t.SampleRatio; r < 0 || r > 1 {
			v.fail("traces.sample_ratio", fmt.Errorf("must be between 0 and 1, not %v", r))
		} else {
			s.Traces.SampleRatio = r
		}
	}
	if s.Traces.MaxExportBatch > s.Traces.MaxQueueSize {
		v.fail("traces.max_export_batch", fmt.Errorf("must not exceed max_queue_size (%d)", s.Traces.MaxQueueSize))
	}
	for _, method := range s.Traces.ExcludeRPC {
		if service, name, ok := strings.Cut(method, "."); !ok || service == "" || name == "" {
			v.fail("traces.exclude_rpc", fmt.Errorf("%q is not of the form Service.Method", method))
		}
	}

	l := c.Logs
	if l == nil {
		l = &LogsConfig{}
	}
	s.Logs = LogsSettings{
		Enabled: boolOr(l.Enabled, false),
		Target:  v.target("logs", c.TargetConfig.merge(l.TargetConfig), boolOr(l.Enabled, false)),
		Level:   v.level("logs.level", l.Level),
		Events:  v.events(l.Events),
	}
	if s.Logs.Events.Enabled && !s.Logs.Enabled {
		v.fail("logs.events.enabled", errors.New("the event export sends log records: it needs logs.enabled = true"))
	}

	if err := v.err(); err != nil {
		return nil, err
	}
	s.Warnings = v.warnings
	return s, nil
}

// validator collects the errors and warnings of one Resolve.
type validator struct {
	errs     []error
	warnings []string
}

func (v *validator) fail(key string, err error) {
	v.errs = append(v.errs, fmt.Errorf("telemetry.otlp.%s: %w", key, err))
}

func (v *validator) warn(key, msg string) {
	v.warnings = append(v.warnings, fmt.Sprintf("telemetry.otlp.%s: %s", key, msg))
}

func (v *validator) err() error {
	return errors.Join(v.errs...)
}

// duration parses a positive duration, or returns def when value is empty.
func (v *validator) duration(key, value string, def time.Duration) time.Duration {
	if value == "" {
		return def
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		v.fail(key, fmt.Errorf("can't parse time duration %q", value))
		return def
	}
	if d <= 0 {
		v.fail(key, fmt.Errorf("must be positive, not %q", value))
		return def
	}
	return d
}

func (v *validator) positive(key string, value, def int) int {
	switch {
	case value == 0:
		return def
	case value < 0:
		v.fail(key, fmt.Errorf("must be positive, not %d", value))
		return def
	default:
		return value
	}
}

func (v *validator) enum(key, value, def string, allowed ...string) string {
	if value == "" {
		return def
	}
	if !slices.Contains(allowed, value) {
		v.fail(key, fmt.Errorf("must be one of %q, not %q", allowed, value))
		return def
	}
	return value
}

func (v *validator) level(key, value string) hclog.Level {
	if value == "" {
		value = DefaultLogsLevel
	}
	level := hclog.LevelFromString(value)
	if level == hclog.NoLevel || level == hclog.Off {
		v.fail(key, fmt.Errorf("must be one of trace, debug, info, warn or error, not %q", value))
		return hclog.Info
	}
	return level
}

// events validates the events block: the topics are names of event topics
// (letters and digits), listed explicitly.
func (v *validator) events(e *EventsConfig) EventsSettings {
	if e == nil {
		e = &EventsConfig{}
	}
	es := EventsSettings{Enabled: boolOr(e.Enabled, false)}
	topics := e.Topics
	if topics == nil {
		topics = DefaultEventTopics
	}
	if len(topics) == 0 {
		v.fail("logs.events.topics", errors.New("must name at least one topic"))
	}
	for _, topic := range topics {
		switch {
		case topic == "*":
			v.fail("logs.events.topics", errors.New(`"*" is not allowed: list the topics to export`))
		case !isTopicName(topic):
			v.fail("logs.events.topics", fmt.Errorf("%q is not a topic name", topic))
		case !slices.Contains(es.Topics, topic):
			es.Topics = append(es.Topics, topic)
		}
	}
	return es
}

// isTopicName reports whether s is a name of an event topic: an ASCII
// letter followed by letters and digits.
func isTopicName(s string) bool {
	for i, r := range s {
		isLetter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		isDigit := r >= '0' && r <= '9'
		if !isLetter && (i == 0 || !isDigit) {
			return false
		}
	}
	return s != ""
}

func (v *validator) retry(r *RetryConfig) RetrySettings {
	if r == nil {
		r = &RetryConfig{}
	}
	rs := RetrySettings{
		Enabled:         boolOr(r.Enabled, true),
		InitialInterval: v.duration("retry.initial_interval", r.InitialInterval, DefaultRetryInitialInterval),
		MaxInterval:     v.duration("retry.max_interval", r.MaxInterval, DefaultRetryMaxInterval),
		MaxElapsed:      v.duration("retry.max_elapsed", r.MaxElapsed, DefaultRetryMaxElapsed),
	}
	if rs.InitialInterval > rs.MaxInterval {
		v.fail("retry.initial_interval", fmt.Errorf("must not exceed max_interval (%s)", rs.MaxInterval))
	}
	return rs
}

func (v *validator) resourceAttributes(attrs map[string]string) map[string]string {
	for k := range attrs {
		switch {
		case strings.TrimSpace(k) == "":
			v.fail("resource_attributes", errors.New("an attribute name is empty"))
		case strings.HasPrefix(k, reservedAttributePrefix):
			v.fail("resource_attributes", fmt.Errorf("%q: the %s* attributes are set by the agent", k, reservedAttributePrefix))
		}
	}
	return maps.Clone(attrs)
}

// target validates the connection settings of one signal, after the
// signal block was merged over the otlp block. A disabled signal is
// validated too, but only an enabled one warns.
func (v *validator) target(signal string, tc TargetConfig, enabled bool) Target {
	t := Target{
		Endpoint:    tc.Endpoint,
		TLS:         tc.TLS.Copy(),
		HeadersFile: tc.HeadersFile,
	}
	if t.Endpoint == "" {
		t.Endpoint = DefaultEndpoint
	}
	key := signal + ".endpoint"
	host, err := endpointHost(t.Endpoint)
	if err != nil {
		v.fail(key, err)
	}
	t.Host = host
	t.Loopback = isLoopback(host)

	insecure := tc.Insecure != nil && *tc.Insecure
	if insecure && tc.TLS != nil {
		v.fail(signal+".insecure", errors.New("insecure = true and a tls block exclude each other"))
	}
	switch {
	case tc.Insecure != nil:
		t.Plaintext = insecure
	default:
		// a loopback endpoint is plaintext unless a tls block asks for TLS
		t.Plaintext = t.Loopback && tc.TLS == nil
	}
	remotePlaintext := t.Plaintext && !t.Loopback && err == nil
	if remotePlaintext && enabled {
		v.warn(signal+".insecure", fmt.Sprintf("exports to %s without TLS", t.Endpoint))
	}

	if tls := tc.TLS; tls != nil && (tls.CertFile == "") != (tls.KeyFile == "") {
		v.fail(signal+".tls", errors.New("cert_file and key_file must be given together"))
	}

	headers, herr := normalizeHeaders(tc.Headers)
	if herr != nil {
		v.fail(signal+".headers", herr)
	}
	t.Headers = headers
	if remotePlaintext && (len(headers) > 0 || t.HeadersFile != "") {
		v.fail(signal+".headers", fmt.Errorf("headers would be sent without TLS to %s", t.Endpoint))
	}
	return t
}

// endpointHost checks the syntax of an endpoint, host:port with an
// optional dns:/// scheme, and returns its host.
func endpointHost(endpoint string) (string, error) {
	hostPort := strings.TrimPrefix(endpoint, dnsScheme)
	if strings.Contains(hostPort, "://") || strings.Contains(hostPort, "/") {
		return "", fmt.Errorf("%q must be host:port or %shost:port", endpoint, dnsScheme)
	}
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return "", fmt.Errorf("%q must be host:port: %w", endpoint, err)
	}
	if host == "" {
		return "", fmt.Errorf("%q has no host", endpoint)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("%q has an invalid port", endpoint)
	}
	return host, nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func boolOr(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"maps"
	"slices"
	"time"

	"github.com/bacanoo-io/norca/norca/helper/pointer"
)

// Defaults of the otlp block.
const (
	DefaultEndpoint             = "127.0.0.1:4317"
	DefaultCompression          = CompressionGzip
	DefaultTimeout              = 10 * time.Second
	DefaultRetryInitialInterval = 5 * time.Second
	DefaultRetryMaxInterval     = 30 * time.Second
	DefaultRetryMaxElapsed      = time.Minute
	DefaultMetricsInterval      = 30 * time.Second
	DefaultTemporality          = TemporalityCumulative
	DefaultHistograms           = HistogramsExplicit
	DefaultSampleRatio          = 0.1
	DefaultMaxQueueSize         = 2048
	DefaultMaxExportBatch       = 512
	DefaultLogsLevel            = "info"
)

// DefaultEventTopics are the event topics the event export sends when its
// topics are not set. The topics that may carry secrets or identities (the
// ACL topics, Variables, Secret, SecretAccess) are exported only when they
// are listed.
var DefaultEventTopics = []string{"Job", "Allocation", "Deployment", "Node", "Scaling", "Backup", "Server"}

// Values of the enumerated keys.
const (
	CompressionGzip = "gzip"
	CompressionNone = "none"

	TemporalityCumulative = "cumulative"
	TemporalityDelta      = "delta"

	HistogramsExplicit    = "explicit"
	HistogramsExponential = "exponential"
)

// OTLPConfig is the otlp block of telemetry: the OTLP/gRPC export of the
// agent's own metrics, traces and (later) logs. Exporting is on as soon as
// the block exists; without it no OpenTelemetry provider is created.
//
//	otlp {
//	  endpoint     = "127.0.0.1:4317"
//	  compression  = "gzip"
//	  timeout      = "10s"
//	  headers      = { "authorization" = "Bearer ..." }
//	  headers_file = "/etc/norca.d/otlp-headers"
//	  tls { ca_file = "/etc/norca.d/otlp-ca.pem" }
//	  retry { max_elapsed = "1m" }
//	  resource_attributes = { "deployment.environment.name" = "production" }
//	  metrics { interval = "30s" }
//	  traces  { sample_ratio = 0.1 }
//	  logs    { enabled = false, events { enabled = false } }
//	}
//
// The durations are strings, parsed by Validate.
type OTLPConfig struct {
	// TargetConfig holds the connection settings each signal may override.
	TargetConfig `hcl:",squash"`

	// Compression is "gzip" (default) or "none".
	Compression string `hcl:"compression"`

	// Timeout bounds one export call, retries included (default 10s).
	Timeout string `hcl:"timeout"`

	// Retry is the retry of a failed export.
	Retry *RetryConfig `hcl:"retry"`

	// ResourceAttributes are added to the resource of every signal. They may
	// not use the norca.* namespace.
	ResourceAttributes map[string]string `hcl:"resource_attributes"`

	Metrics *MetricsConfig `hcl:"metrics"`
	Traces  *TracesConfig  `hcl:"traces"`
	Logs    *LogsConfig    `hcl:"logs"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// TargetConfig is where and how a signal is sent: the otlp block sets it
// for every signal and each signal block may override it key by key. The
// transport security (insecure or a tls block) counts as one key: a signal
// that sets either of them replaces both.
type TargetConfig struct {
	// Endpoint is host:port or dns:///host:port of an OTLP/gRPC receiver
	// (default 127.0.0.1:4317, the collector of the node).
	Endpoint string `hcl:"endpoint"`

	// Insecure sends in plaintext. A loopback endpoint is plaintext unless a
	// tls block is given; any other endpoint uses TLS unless insecure = true.
	Insecure *bool `hcl:"insecure"`

	// Headers are sent as gRPC metadata with every export (authentication).
	Headers map[string]string `hcl:"headers"`

	// HeadersFile holds more headers, key=value per line. It is read at
	// startup and again on SIGHUP; its values win over Headers.
	HeadersFile string `hcl:"headers_file"`

	// TLS sets the CA, the server name and the client certificate (mTLS).
	TLS *TLSConfig `hcl:"tls"`
}

// TLSConfig is the tls block of the otlp block or of a signal block.
type TLSConfig struct {
	// CAFile verifies the receiver; without it the system roots do.
	CAFile string `hcl:"ca_file"`

	// CertFile and KeyFile are the client certificate for mTLS.
	CertFile string `hcl:"cert_file"`
	KeyFile  string `hcl:"key_file"`

	// ServerName overrides the name the receiver's certificate is checked
	// against (default: the host of the endpoint).
	ServerName string `hcl:"server_name"`

	// InsecureSkipVerify turns off the verification of the receiver.
	InsecureSkipVerify bool `hcl:"insecure_skip_verify"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// RetryConfig is the retry of a failed export (the exporters' RetryConfig).
type RetryConfig struct {
	Enabled         *bool  `hcl:"enabled"`
	InitialInterval string `hcl:"initial_interval"`
	MaxInterval     string `hcl:"max_interval"`

	// MaxElapsed bounds the retries of one batch; then it is dropped and
	// counted.
	MaxElapsed string `hcl:"max_elapsed"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// MetricsConfig is the metrics block of the otlp block.
type MetricsConfig struct {
	TargetConfig `hcl:",squash"`

	// Enabled defaults to true once the otlp block exists.
	Enabled *bool `hcl:"enabled"`

	// Interval is the export interval of the periodic reader (default 30s).
	Interval string `hcl:"interval"`

	// Temporality is "cumulative" (default) or "delta".
	Temporality string `hcl:"temporality"`

	// Histograms is "explicit" (default) or "exponential".
	Histograms string `hcl:"histograms"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// TracesConfig is the traces block of the otlp block.
type TracesConfig struct {
	TargetConfig `hcl:",squash"`

	// Enabled defaults to true once the otlp block exists.
	Enabled *bool `hcl:"enabled"`

	// SampleRatio is the ratio of new root traces that are sampled
	// (default 0.1), between 0 and 1.
	SampleRatio *float64 `hcl:"sample_ratio"`

	// TrustHTTPParent makes the trace context of HTTP API requests the
	// parent of the agent's spans instead of a link.
	TrustHTTPParent *bool `hcl:"trust_http_parent"`

	// ExcludeRPC lists RPC methods without spans, on top of the built-in
	// list.
	ExcludeRPC []string `hcl:"exclude_rpc"`

	// MaxQueueSize and MaxExportBatch size the batch span processor.
	MaxQueueSize   int `hcl:"max_queue_size"`
	MaxExportBatch int `hcl:"max_export_batch"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// LogsConfig is the logs block of the otlp block.
type LogsConfig struct {
	TargetConfig `hcl:",squash"`

	// Enabled defaults to false.
	Enabled *bool `hcl:"enabled"`

	// Level is the minimum level exported (default "info"), independent of
	// log_level.
	Level string `hcl:"level"`

	// Events is the export of the event stream of the region as log
	// records, by the leader.
	Events *EventsConfig `hcl:"events"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// EventsConfig is the events block of the logs block: the leader of the
// region sends the events of the event stream as OTLP log records. It runs
// on servers only and needs the logs signal.
//
//	events {
//	  enabled = true
//	  topics  = ["Job", "Allocation", "Deployment", "Node", "Scaling", "Backup", "Server"]
//	}
type EventsConfig struct {
	// Enabled defaults to false.
	Enabled *bool `hcl:"enabled"`

	// Topics are the exported topics (default DefaultEventTopics). A topic
	// is exported only when it is listed: there is no wildcard.
	Topics []string `hcl:"topics"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// Copy returns a deep copy.
func (c *OTLPConfig) Copy() *OTLPConfig {
	if c == nil {
		return nil
	}
	nc := *c
	nc.TargetConfig = c.TargetConfig.copy()
	nc.Retry = c.Retry.Copy()
	nc.ResourceAttributes = maps.Clone(c.ResourceAttributes)
	nc.Metrics = c.Metrics.Copy()
	nc.Traces = c.Traces.Copy()
	nc.Logs = c.Logs.Copy()
	nc.ExtraKeysHCL = slices.Clone(c.ExtraKeysHCL)
	return &nc
}

// Merge merges b into a copy of c, key by key: a key set in b replaces the
// one of c; maps and lists are replaced as a whole.
func (c *OTLPConfig) Merge(b *OTLPConfig) *OTLPConfig {
	if c == nil {
		return b.Copy()
	}
	result := c.Copy()
	if b == nil {
		return result
	}
	result.TargetConfig = result.TargetConfig.merge(b.TargetConfig)
	if b.Compression != "" {
		result.Compression = b.Compression
	}
	if b.Timeout != "" {
		result.Timeout = b.Timeout
	}
	result.Retry = result.Retry.Merge(b.Retry)
	if b.ResourceAttributes != nil {
		result.ResourceAttributes = maps.Clone(b.ResourceAttributes)
	}
	result.Metrics = result.Metrics.Merge(b.Metrics)
	result.Traces = result.Traces.Merge(b.Traces)
	result.Logs = result.Logs.Merge(b.Logs)
	return result
}

// Redact replaces the values of the headers, which hold credentials, for
// the agent self endpoint. The header names stay visible.
func (c *OTLPConfig) Redact() {
	if c == nil {
		return
	}
	c.TargetConfig.redact()
	if c.Metrics != nil {
		c.Metrics.TargetConfig.redact()
	}
	if c.Traces != nil {
		c.Traces.TargetConfig.redact()
	}
	if c.Logs != nil {
		c.Logs.TargetConfig.redact()
	}
}

// redactedValue replaces a secret in the agent self endpoint.
const redactedValue = "<redacted>"

func (t *TargetConfig) redact() {
	for k := range t.Headers {
		t.Headers[k] = redactedValue
	}
}

func (t TargetConfig) copy() TargetConfig {
	nt := t
	nt.Insecure = pointer.Copy(t.Insecure)
	nt.Headers = maps.Clone(t.Headers)
	nt.TLS = t.TLS.Copy()
	return nt
}

// setsTransport reports whether the target sets the transport security.
func (t TargetConfig) setsTransport() bool {
	return t.Insecure != nil || t.TLS != nil
}

// merge returns t with the keys b sets. insecure and tls are merged as one
// key, so a later file can switch from one to the other.
func (t TargetConfig) merge(b TargetConfig) TargetConfig {
	result := t.copy()
	if b.Endpoint != "" {
		result.Endpoint = b.Endpoint
	}
	if b.setsTransport() {
		result.Insecure = pointer.Copy(b.Insecure)
		result.TLS = b.TLS.Copy()
	}
	if b.Headers != nil {
		result.Headers = maps.Clone(b.Headers)
	}
	if b.HeadersFile != "" {
		result.HeadersFile = b.HeadersFile
	}
	return result
}

// Copy returns a deep copy.
func (t *TLSConfig) Copy() *TLSConfig {
	if t == nil {
		return nil
	}
	nt := *t
	nt.ExtraKeysHCL = slices.Clone(t.ExtraKeysHCL)
	return &nt
}

// Copy returns a deep copy.
func (r *RetryConfig) Copy() *RetryConfig {
	if r == nil {
		return nil
	}
	nr := *r
	nr.Enabled = pointer.Copy(r.Enabled)
	nr.ExtraKeysHCL = slices.Clone(r.ExtraKeysHCL)
	return &nr
}

// Merge merges b into a copy of r, key by key.
func (r *RetryConfig) Merge(b *RetryConfig) *RetryConfig {
	if r == nil {
		return b.Copy()
	}
	result := r.Copy()
	if b == nil {
		return result
	}
	if b.Enabled != nil {
		result.Enabled = pointer.Copy(b.Enabled)
	}
	if b.InitialInterval != "" {
		result.InitialInterval = b.InitialInterval
	}
	if b.MaxInterval != "" {
		result.MaxInterval = b.MaxInterval
	}
	if b.MaxElapsed != "" {
		result.MaxElapsed = b.MaxElapsed
	}
	return result
}

// Copy returns a deep copy.
func (m *MetricsConfig) Copy() *MetricsConfig {
	if m == nil {
		return nil
	}
	nm := *m
	nm.TargetConfig = m.TargetConfig.copy()
	nm.Enabled = pointer.Copy(m.Enabled)
	nm.ExtraKeysHCL = slices.Clone(m.ExtraKeysHCL)
	return &nm
}

// Merge merges b into a copy of m, key by key.
func (m *MetricsConfig) Merge(b *MetricsConfig) *MetricsConfig {
	if m == nil {
		return b.Copy()
	}
	result := m.Copy()
	if b == nil {
		return result
	}
	result.TargetConfig = result.TargetConfig.merge(b.TargetConfig)
	if b.Enabled != nil {
		result.Enabled = pointer.Copy(b.Enabled)
	}
	if b.Interval != "" {
		result.Interval = b.Interval
	}
	if b.Temporality != "" {
		result.Temporality = b.Temporality
	}
	if b.Histograms != "" {
		result.Histograms = b.Histograms
	}
	return result
}

// Copy returns a deep copy.
func (t *TracesConfig) Copy() *TracesConfig {
	if t == nil {
		return nil
	}
	nt := *t
	nt.TargetConfig = t.TargetConfig.copy()
	nt.Enabled = pointer.Copy(t.Enabled)
	nt.SampleRatio = pointer.Copy(t.SampleRatio)
	nt.TrustHTTPParent = pointer.Copy(t.TrustHTTPParent)
	nt.ExcludeRPC = slices.Clone(t.ExcludeRPC)
	nt.ExtraKeysHCL = slices.Clone(t.ExtraKeysHCL)
	return &nt
}

// Merge merges b into a copy of t, key by key.
func (t *TracesConfig) Merge(b *TracesConfig) *TracesConfig {
	if t == nil {
		return b.Copy()
	}
	result := t.Copy()
	if b == nil {
		return result
	}
	result.TargetConfig = result.TargetConfig.merge(b.TargetConfig)
	if b.Enabled != nil {
		result.Enabled = pointer.Copy(b.Enabled)
	}
	if b.SampleRatio != nil {
		result.SampleRatio = pointer.Copy(b.SampleRatio)
	}
	if b.TrustHTTPParent != nil {
		result.TrustHTTPParent = pointer.Copy(b.TrustHTTPParent)
	}
	if b.ExcludeRPC != nil {
		result.ExcludeRPC = slices.Clone(b.ExcludeRPC)
	}
	if b.MaxQueueSize != 0 {
		result.MaxQueueSize = b.MaxQueueSize
	}
	if b.MaxExportBatch != 0 {
		result.MaxExportBatch = b.MaxExportBatch
	}
	return result
}

// Copy returns a deep copy.
func (l *LogsConfig) Copy() *LogsConfig {
	if l == nil {
		return nil
	}
	nl := *l
	nl.TargetConfig = l.TargetConfig.copy()
	nl.Enabled = pointer.Copy(l.Enabled)
	nl.Events = l.Events.Copy()
	nl.ExtraKeysHCL = slices.Clone(l.ExtraKeysHCL)
	return &nl
}

// Merge merges b into a copy of l, key by key.
func (l *LogsConfig) Merge(b *LogsConfig) *LogsConfig {
	if l == nil {
		return b.Copy()
	}
	result := l.Copy()
	if b == nil {
		return result
	}
	result.TargetConfig = result.TargetConfig.merge(b.TargetConfig)
	if b.Enabled != nil {
		result.Enabled = pointer.Copy(b.Enabled)
	}
	if b.Level != "" {
		result.Level = b.Level
	}
	result.Events = result.Events.Merge(b.Events)
	return result
}

// Copy returns a deep copy.
func (e *EventsConfig) Copy() *EventsConfig {
	if e == nil {
		return nil
	}
	ne := *e
	ne.Enabled = pointer.Copy(e.Enabled)
	ne.Topics = slices.Clone(e.Topics)
	ne.ExtraKeysHCL = slices.Clone(e.ExtraKeysHCL)
	return &ne
}

// Merge merges b into a copy of e, key by key; the topics are replaced as a
// whole.
func (e *EventsConfig) Merge(b *EventsConfig) *EventsConfig {
	if e == nil {
		return b.Copy()
	}
	result := e.Copy()
	if b == nil {
		return result
	}
	if b.Enabled != nil {
		result.Enabled = pointer.Copy(b.Enabled)
	}
	if b.Topics != nil {
		result.Topics = slices.Clone(b.Topics)
	}
	return result
}

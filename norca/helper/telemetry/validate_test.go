// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/hashicorp/go-hclog"
	"github.com/shoenig/test/must"
)

func TestResolve_Nil(t *testing.T) {
	ci.Parallel(t)

	s, err := Resolve(nil)
	must.NoError(t, err)
	must.Nil(t, s)
}

func TestResolve_Defaults(t *testing.T) {
	ci.Parallel(t)

	s, err := Resolve(&OTLPConfig{})
	must.NoError(t, err)

	local := Target{
		Endpoint:  DefaultEndpoint,
		Host:      "127.0.0.1",
		Loopback:  true,
		Plaintext: true,
		Headers:   map[string]string{},
	}
	must.Eq(t, &Settings{
		Compression: CompressionGzip,
		Timeout:     10 * time.Second,
		Retry: RetrySettings{
			Enabled:         true,
			InitialInterval: 5 * time.Second,
			MaxInterval:     30 * time.Second,
			MaxElapsed:      time.Minute,
		},
		Metrics: MetricsSettings{
			Enabled:     true,
			Target:      local,
			Interval:    30 * time.Second,
			Temporality: TemporalityCumulative,
			Histograms:  HistogramsExplicit,
		},
		Traces: TracesSettings{
			Enabled:        true,
			Target:         local,
			SampleRatio:    0.1,
			MaxQueueSize:   2048,
			MaxExportBatch: 512,
		},
		Logs: LogsSettings{
			Enabled: false,
			Target:  local,
			Level:   hclog.Info,
			Events: EventsSettings{
				Topics: []string{"Job", "Allocation", "Deployment", "Node", "Scaling", "Backup", "Server"},
			},
		},
	}, s)
}

func TestResolve_Keys(t *testing.T) {
	ci.Parallel(t)

	c := &OTLPConfig{
		TargetConfig: TargetConfig{
			Endpoint:    "dns:///otlp.example.com:4317",
			Headers:     map[string]string{"Authorization": "Bearer x", "x-api-key": "k"},
			HeadersFile: "/etc/norca.d/otlp-headers",
			TLS: &TLSConfig{
				CAFile:     "/ca.pem",
				CertFile:   "/c.pem",
				KeyFile:    "/k.pem",
				ServerName: "collector",
			},
		},
		Compression: "none",
		Timeout:     "3s",
		Retry: &RetryConfig{
			Enabled:         pointer.Of(false),
			InitialInterval: "1s",
			MaxInterval:     "2s",
			MaxElapsed:      "4s",
		},
		ResourceAttributes: map[string]string{"deployment.environment.name": "prod"},
		Metrics: &MetricsConfig{
			Enabled:     pointer.Of(false),
			Interval:    "5s",
			Temporality: "delta",
			Histograms:  "exponential",
		},
		Traces: &TracesConfig{
			Enabled:         pointer.Of(false),
			SampleRatio:     pointer.Of(0.0),
			TrustHTTPParent: pointer.Of(true),
			ExcludeRPC:      []string{"Node.GetClientAllocs"},
			MaxQueueSize:    100,
			MaxExportBatch:  10,
		},
		Logs: &LogsConfig{
			Enabled: pointer.Of(true),
			Level:   "warn",
			Events: &EventsConfig{
				Enabled: pointer.Of(true),
				Topics:  []string{"Job", "Secret", "Job", "ACLToken"},
			},
		},
	}
	s, err := Resolve(c)
	must.NoError(t, err)

	must.Eq(t, CompressionNone, s.Compression)
	must.Eq(t, 3*time.Second, s.Timeout)
	must.Eq(t, RetrySettings{InitialInterval: time.Second, MaxInterval: 2 * time.Second, MaxElapsed: 4 * time.Second}, s.Retry)
	must.Eq(t, map[string]string{"deployment.environment.name": "prod"}, s.ResourceAttributes)
	must.Eq(t, MetricsSettings{
		Target:      s.Metrics.Target,
		Interval:    5 * time.Second,
		Temporality: TemporalityDelta,
		Histograms:  HistogramsExponential,
	}, s.Metrics)
	must.Eq(t, TracesSettings{
		Target:          s.Traces.Target,
		SampleRatio:     0,
		TrustHTTPParent: true,
		ExcludeRPC:      []string{"Node.GetClientAllocs"},
		MaxQueueSize:    100,
		MaxExportBatch:  10,
	}, s.Traces)
	must.True(t, s.Logs.Enabled)
	must.Eq(t, hclog.Warn, s.Logs.Level)
	must.Eq(t, EventsSettings{Enabled: true, Topics: []string{"Job", "Secret", "ACLToken"}}, s.Logs.Events)

	target := s.Traces.Target
	must.Eq(t, "dns:///otlp.example.com:4317", target.Endpoint)
	must.Eq(t, "otlp.example.com", target.Host)
	must.False(t, target.Loopback)
	must.False(t, target.Plaintext)
	must.Eq(t, "collector", target.TLS.ServerName)
	must.Eq(t, map[string]string{"authorization": "Bearer x", "x-api-key": "k"}, target.Headers)
	must.Eq(t, "/etc/norca.d/otlp-headers", target.HeadersFile)
	must.Eq(t, target, s.Metrics.Target)
	must.Eq(t, target, s.Logs.Target)
	must.SliceEmpty(t, s.Warnings)
}

func TestResolve_Transport(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name      string
		target    TargetConfig
		plaintext bool
		loopback  bool
		warns     bool
		wantErr   string
	}{
		{name: "loopback is plaintext", target: TargetConfig{Endpoint: "127.0.0.1:4317"}, plaintext: true, loopback: true},
		{name: "other loopback address", target: TargetConfig{Endpoint: "127.0.0.2:4317"}, plaintext: true, loopback: true},
		{name: "localhost", target: TargetConfig{Endpoint: "localhost:4317"}, plaintext: true, loopback: true},
		{name: "ipv6 loopback", target: TargetConfig{Endpoint: "[::1]:4317"}, plaintext: true, loopback: true},
		{name: "loopback with tls", target: TargetConfig{Endpoint: "127.0.0.1:4317", TLS: &TLSConfig{}}, loopback: true},
		{name: "loopback insecure false", target: TargetConfig{Endpoint: "127.0.0.1:4317", Insecure: pointer.Of(false)}, loopback: true},
		{name: "remote is tls", target: TargetConfig{Endpoint: "10.0.0.1:4317"}},
		{name: "remote dns name is tls", target: TargetConfig{Endpoint: "otlp.example.com:4317"}},
		{name: "remote insecure warns", target: TargetConfig{Endpoint: "10.0.0.1:4317", Insecure: pointer.Of(true)}, plaintext: true, warns: true},
		{
			name:    "insecure and tls",
			target:  TargetConfig{Endpoint: "10.0.0.1:4317", Insecure: pointer.Of(true), TLS: &TLSConfig{}},
			wantErr: "exclude each other",
		},
		{
			name:    "headers over remote plaintext",
			target:  TargetConfig{Endpoint: "10.0.0.1:4317", Insecure: pointer.Of(true), Headers: map[string]string{"a": "b"}},
			wantErr: "without TLS",
		},
		{
			name:    "headers file over remote plaintext",
			target:  TargetConfig{Endpoint: "10.0.0.1:4317", Insecure: pointer.Of(true), HeadersFile: "/h"},
			wantErr: "without TLS",
		},
		{
			name:      "headers over loopback plaintext",
			target:    TargetConfig{Endpoint: "127.0.0.1:4317", Headers: map[string]string{"a": "b"}},
			plaintext: true,
			loopback:  true,
		},
		{
			name:    "cert without key",
			target:  TargetConfig{Endpoint: "10.0.0.1:4317", TLS: &TLSConfig{CertFile: "/c.pem"}},
			wantErr: "cert_file and key_file",
		},
		{
			name:    "key without cert",
			target:  TargetConfig{Endpoint: "10.0.0.1:4317", TLS: &TLSConfig{KeyFile: "/k.pem"}},
			wantErr: "cert_file and key_file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := Resolve(&OTLPConfig{TargetConfig: tt.target})
			if tt.wantErr != "" {
				must.ErrorContains(t, err, tt.wantErr)
				return
			}
			must.NoError(t, err)
			must.Eq(t, tt.plaintext, s.Traces.Target.Plaintext)
			must.Eq(t, tt.loopback, s.Traces.Target.Loopback)
			if tt.warns {
				// one warning per enabled signal (logs are off)
				must.Len(t, 2, s.Warnings)
				must.StrContains(t, s.Warnings[0], "without TLS")
			} else {
				must.SliceEmpty(t, s.Warnings)
			}
		})
	}
}

func TestResolve_Errors(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name    string
		config  *OTLPConfig
		wantErr string
	}{
		{name: "endpoint without port", config: &OTLPConfig{TargetConfig: TargetConfig{Endpoint: "collector"}}, wantErr: "telemetry.otlp.metrics.endpoint"},
		{name: "endpoint bad port", config: &OTLPConfig{TargetConfig: TargetConfig{Endpoint: "collector:99999"}}, wantErr: "invalid port"},
		{name: "endpoint no host", config: &OTLPConfig{TargetConfig: TargetConfig{Endpoint: ":4317"}}, wantErr: "no host"},
		{name: "endpoint url", config: &OTLPConfig{TargetConfig: TargetConfig{Endpoint: "http://collector:4317"}}, wantErr: "must be host:port"},
		{name: "endpoint path", config: &OTLPConfig{TargetConfig: TargetConfig{Endpoint: "collector:4317/v1"}}, wantErr: "must be host:port"},
		{name: "compression", config: &OTLPConfig{Compression: "zstd"}, wantErr: "telemetry.otlp.compression"},
		{name: "timeout syntax", config: &OTLPConfig{Timeout: "soon"}, wantErr: "telemetry.otlp.timeout"},
		{name: "timeout zero", config: &OTLPConfig{Timeout: "0s"}, wantErr: "must be positive"},
		{name: "retry initial", config: &OTLPConfig{Retry: &RetryConfig{InitialInterval: "x"}}, wantErr: "retry.initial_interval"},
		{name: "retry max", config: &OTLPConfig{Retry: &RetryConfig{MaxInterval: "-1s"}}, wantErr: "retry.max_interval"},
		{name: "retry elapsed", config: &OTLPConfig{Retry: &RetryConfig{MaxElapsed: "x"}}, wantErr: "retry.max_elapsed"},
		{name: "retry order", config: &OTLPConfig{Retry: &RetryConfig{InitialInterval: "1m", MaxInterval: "1s"}}, wantErr: "must not exceed max_interval"},
		{name: "resource norca key", config: &OTLPConfig{ResourceAttributes: map[string]string{"norca.node.id": "x"}}, wantErr: "set by the agent"},
		{name: "resource empty key", config: &OTLPConfig{ResourceAttributes: map[string]string{" ": "x"}}, wantErr: "empty"},
		{name: "metrics interval", config: &OTLPConfig{Metrics: &MetricsConfig{Interval: "0s"}}, wantErr: "metrics.interval"},
		{name: "metrics temporality", config: &OTLPConfig{Metrics: &MetricsConfig{Temporality: "both"}}, wantErr: "metrics.temporality"},
		{name: "metrics histograms", config: &OTLPConfig{Metrics: &MetricsConfig{Histograms: "summary"}}, wantErr: "metrics.histograms"},
		{name: "sample ratio high", config: &OTLPConfig{Traces: &TracesConfig{SampleRatio: pointer.Of(1.5)}}, wantErr: "traces.sample_ratio"},
		{name: "sample ratio negative", config: &OTLPConfig{Traces: &TracesConfig{SampleRatio: pointer.Of(-0.1)}}, wantErr: "traces.sample_ratio"},
		{name: "queue size", config: &OTLPConfig{Traces: &TracesConfig{MaxQueueSize: -1}}, wantErr: "traces.max_queue_size"},
		{name: "batch size", config: &OTLPConfig{Traces: &TracesConfig{MaxExportBatch: -1}}, wantErr: "traces.max_export_batch"},
		{name: "batch over queue", config: &OTLPConfig{Traces: &TracesConfig{MaxQueueSize: 10, MaxExportBatch: 20}}, wantErr: "must not exceed max_queue_size"},
		{name: "exclude rpc", config: &OTLPConfig{Traces: &TracesConfig{ExcludeRPC: []string{"Ping"}}}, wantErr: "traces.exclude_rpc"},
		{name: "logs level", config: &OTLPConfig{Logs: &LogsConfig{Level: "loud"}}, wantErr: "logs.level"},
		{name: "logs level off", config: &OTLPConfig{Logs: &LogsConfig{Level: "off"}}, wantErr: "logs.level"},
		{
			name:    "events without logs",
			config:  &OTLPConfig{Logs: &LogsConfig{Events: &EventsConfig{Enabled: pointer.Of(true)}}},
			wantErr: "logs.events.enabled: the event export sends log records: it needs logs.enabled = true",
		},
		{
			name:    "events wildcard",
			config:  &OTLPConfig{Logs: &LogsConfig{Events: &EventsConfig{Topics: []string{"*"}}}},
			wantErr: "logs.events.topics",
		},
		{
			name:    "events topic name",
			config:  &OTLPConfig{Logs: &LogsConfig{Events: &EventsConfig{Topics: []string{"Job.x"}}}},
			wantErr: `"Job.x" is not a topic name`,
		},
		{
			name:    "events no topics",
			config:  &OTLPConfig{Logs: &LogsConfig{Events: &EventsConfig{Topics: []string{}}}},
			wantErr: "must name at least one topic",
		},
		{name: "header reserved", config: &OTLPConfig{TargetConfig: TargetConfig{Headers: map[string]string{"grpc-timeout": "1"}}}, wantErr: "reserved"},
		{name: "header binary", config: &OTLPConfig{TargetConfig: TargetConfig{Headers: map[string]string{"x-bin": "1"}}}, wantErr: "binary"},
		{name: "header name", config: &OTLPConfig{TargetConfig: TargetConfig{Headers: map[string]string{"bad name": "1"}}}, wantErr: "character"},
		{name: "header value", config: &OTLPConfig{TargetConfig: TargetConfig{Headers: map[string]string{"a": "line\nbreak"}}}, wantErr: "value of header"},
		{name: "header twice", config: &OTLPConfig{TargetConfig: TargetConfig{Headers: map[string]string{"A": "1", "a": "2"}}}, wantErr: "twice"},
		{
			name:    "signal endpoint",
			config:  &OTLPConfig{Traces: &TracesConfig{TargetConfig: TargetConfig{Endpoint: "x"}}},
			wantErr: "telemetry.otlp.traces.endpoint",
		},
		{
			name:    "signal insecure and tls",
			config:  &OTLPConfig{Logs: &LogsConfig{TargetConfig: TargetConfig{Insecure: pointer.Of(true), TLS: &TLSConfig{}}}},
			wantErr: "telemetry.otlp.logs.insecure",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Resolve(tt.config)
			must.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestResolve_ErrorsHideHeaderValues(t *testing.T) {
	ci.Parallel(t)

	const secret = "s3cr3t-value"
	_, err := Resolve(&OTLPConfig{TargetConfig: TargetConfig{
		Endpoint: "10.0.0.1:4317",
		Insecure: pointer.Of(true),
		Headers:  map[string]string{"authorization": secret, "bad name": secret + "\n"},
	}})
	must.Error(t, err)
	must.StrNotContains(t, err.Error(), secret)
}

// A signal block overrides the connection settings of the otlp block key by
// key; insecure and tls count as one key.
func TestResolve_SignalOverrides(t *testing.T) {
	ci.Parallel(t)

	c := &OTLPConfig{
		TargetConfig: TargetConfig{
			Endpoint:    "central.example.com:4317",
			Headers:     map[string]string{"authorization": "Basic a"},
			HeadersFile: "/etc/norca.d/otlp-headers",
			TLS:         &TLSConfig{CAFile: "/central-ca.pem", ServerName: "central"},
		},
		Traces: &TracesConfig{TargetConfig: TargetConfig{
			Endpoint: "tempo.example.com:4317",
			TLS:      &TLSConfig{CAFile: "/tempo-ca.pem"},
		}},
		Metrics: &MetricsConfig{TargetConfig: TargetConfig{
			Endpoint: "127.0.0.1:4317",
			Insecure: pointer.Of(true),
			Headers:  map[string]string{},
		}},
		Logs: &LogsConfig{TargetConfig: TargetConfig{
			Headers: map[string]string{"x-scope": "logs"},
		}},
	}
	s, err := Resolve(c)
	must.NoError(t, err)

	traces := s.Traces.Target
	must.Eq(t, "tempo.example.com:4317", traces.Endpoint)
	must.False(t, traces.Plaintext)
	must.Eq(t, &TLSConfig{CAFile: "/tempo-ca.pem"}, traces.TLS, must.Sprint("the tls block is replaced, not merged"))
	must.Eq(t, map[string]string{"authorization": "Basic a"}, traces.Headers)
	must.Eq(t, "/etc/norca.d/otlp-headers", traces.HeadersFile)

	metrics := s.Metrics.Target
	must.True(t, metrics.Plaintext)
	must.Nil(t, metrics.TLS, must.Sprint("insecure replaces the inherited tls block"))
	must.MapEmpty(t, metrics.Headers)
	must.Eq(t, "/etc/norca.d/otlp-headers", metrics.HeadersFile)

	logs := s.Logs.Target
	must.Eq(t, "central.example.com:4317", logs.Endpoint)
	must.Eq(t, "central", logs.TLS.ServerName)
	must.Eq(t, map[string]string{"x-scope": "logs"}, logs.Headers)
}

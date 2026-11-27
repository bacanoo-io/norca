// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/helper/telemetry"
	"github.com/shoenig/test/must"
)

func TestPrometheusConfig_ExpirationDuration(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name     string
		config   *PrometheusConfig
		expected time.Duration
		wantErr  string
	}{
		{name: "nil block", config: nil, expected: DefaultPrometheusExpiration},
		{name: "unset", config: &PrometheusConfig{}, expected: DefaultPrometheusExpiration},
		{name: "set", config: &PrometheusConfig{Expiration: "2m"}, expected: 2 * time.Minute},
		{name: "zero keeps forever", config: &PrometheusConfig{Expiration: "0s"}, expected: 0},
		{name: "bad syntax", config: &PrometheusConfig{Expiration: "soon"}, wantErr: "can't parse"},
		{name: "negative", config: &PrometheusConfig{Expiration: "-1s"}, wantErr: "must not be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.config.ExpirationDuration()
			if tt.wantErr != "" {
				must.ErrorContains(t, err, tt.wantErr)
				return
			}
			must.NoError(t, err)
			must.Eq(t, tt.expected, got)
		})
	}
}

func TestPrometheusConfig_Merge(t *testing.T) {
	ci.Parallel(t)

	on := &PrometheusConfig{Enabled: pointer.Of(true), Expiration: "30s"}

	t.Run("nil base", func(t *testing.T) {
		got := (*PrometheusConfig)(nil).Merge(on)
		must.Eq(t, on, got)
		must.NotEqOp(t, on, got)
	})
	t.Run("nil overlay", func(t *testing.T) {
		must.Eq(t, on, on.Merge(nil))
	})
	t.Run("later file turns it off and keeps the expiration", func(t *testing.T) {
		got := on.Merge(&PrometheusConfig{Enabled: pointer.Of(false)})
		must.False(t, got.IsEnabled())
		must.Eq(t, "30s", got.Expiration)
		must.True(t, on.IsEnabled())
	})
	t.Run("telemetry merge", func(t *testing.T) {
		a := &Telemetry{Prometheus: on}
		b := &Telemetry{Prometheus: &PrometheusConfig{Expiration: "5s"}}
		got := a.Merge(b)
		must.True(t, got.PrometheusEnabled())
		must.Eq(t, "5s", got.Prometheus.Expiration)
		must.Eq(t, "30s", on.Expiration)
	})
}

func TestTelemetry_Copy_Prometheus(t *testing.T) {
	ci.Parallel(t)

	a := &Telemetry{Prometheus: &PrometheusConfig{Enabled: pointer.Of(true)}}
	b := a.Copy()
	*b.Prometheus.Enabled = false
	must.True(t, a.PrometheusEnabled())
}

func TestTelemetry_PrometheusEnabled(t *testing.T) {
	ci.Parallel(t)

	must.False(t, (*Telemetry)(nil).PrometheusEnabled())
	must.False(t, DefaultConfig().Telemetry.PrometheusEnabled())
	must.True(t, DevConfig(nil).Telemetry.PrometheusEnabled())
}

func TestConfig_Parse_Prometheus(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name    string
		hcl     string
		want    *PrometheusConfig
		wantErr string
	}{
		{
			name: "block",
			hcl:  `telemetry { prometheus { enabled = true expiration = "90s" } }`,
			want: &PrometheusConfig{Enabled: pointer.Of(true), Expiration: "90s"},
		},
		{
			// the only member of telemetry: hcl reads it as a labelled block
			name: "json only member",
			hcl:  `{"telemetry": {"prometheus": {"enabled": true}}}`,
			want: &PrometheusConfig{Enabled: pointer.Of(true)},
		},
		{
			name:    "bad expiration",
			hcl:     `telemetry { prometheus { expiration = "x" } }`,
			wantErr: "telemetry.prometheus.expiration",
		},
		{
			name:    "unknown key in the block",
			hcl:     `telemetry { prometheus { enable = true } }`,
			wantErr: "enable",
		},
		{
			name:    "old key is gone",
			hcl:     `telemetry { prometheus_metrics = true }`,
			wantErr: "prometheus_metrics",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.hcl")
			must.NoError(t, os.WriteFile(path, []byte(tt.hcl), 0o600))
			c, err := ParseConfigFile(path)
			if tt.wantErr != "" {
				must.ErrorContains(t, err, tt.wantErr)
				return
			}
			must.NoError(t, err)
			must.Eq(t, tt.want, c.Telemetry.Prometheus)
		})
	}
}

func parseConfigString(t *testing.T, name, content string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	must.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return ParseConfigFile(path)
}

// The JSON reader of hcl leaves the names of nested objects behind; every
// shape of the otlp block parses without unexpected keys.
func TestConfig_Parse_OTLP_JSONShapes(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name string
		json string
		want *telemetry.OTLPConfig
	}{
		{
			name: "only member",
			json: `{"telemetry":{"otlp":{"endpoint":"x:1"}}}`,
			want: &telemetry.OTLPConfig{TargetConfig: telemetry.TargetConfig{Endpoint: "x:1"}},
		},
		{
			name: "tls only",
			json: `{"telemetry":{"otlp":{"tls":{"ca_file":"/ca"}}}}`,
			want: &telemetry.OTLPConfig{TargetConfig: telemetry.TargetConfig{TLS: &telemetry.TLSConfig{CAFile: "/ca"}}},
		},
		{
			name: "signal tls only",
			json: `{"telemetry":{"otlp":{"traces":{"tls":{"ca_file":"/ca"}}}}}`,
			want: &telemetry.OTLPConfig{Traces: &telemetry.TracesConfig{
				TargetConfig: telemetry.TargetConfig{TLS: &telemetry.TLSConfig{CAFile: "/ca"}},
			}},
		},
		{
			name: "headers only",
			json: `{"telemetry":{"otlp":{"headers":{"a":"b"}}}}`,
			want: &telemetry.OTLPConfig{TargetConfig: telemetry.TargetConfig{Headers: map[string]string{"a": "b"}}},
		},
		{
			name: "signal headers",
			json: `{"telemetry":{"otlp":{"logs":{"headers":{"a":"b"}}}}}`,
			want: &telemetry.OTLPConfig{Logs: &telemetry.LogsConfig{
				TargetConfig: telemetry.TargetConfig{Headers: map[string]string{"a": "b"}},
			}},
		},
		{
			name: "next to other keys",
			json: `{"telemetry":{"collection_interval":"1s","otlp":{"retry":{"enabled":true},"resource_attributes":{"k":"v"},"metrics":{"interval":"5s"},"traces":{"exclude_rpc":["A.B"]}}}}`,
			want: &telemetry.OTLPConfig{
				Retry:              &telemetry.RetryConfig{Enabled: pointer.Of(true)},
				ResourceAttributes: map[string]string{"k": "v"},
				Metrics:            &telemetry.MetricsConfig{Interval: "5s"},
				Traces:             &telemetry.TracesConfig{ExcludeRPC: []string{"A.B"}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := parseConfigString(t, "agent.json", tt.json)
			must.NoError(t, err)
			clearExtraKeys(c.Telemetry.OTLP)
			must.Eq(t, tt.want, c.Telemetry.OTLP)
		})
	}
}

// clearExtraKeys drops the (empty) unused key lists hcl leaves in the
// nested blocks, so a test can compare them with a literal.
func clearExtraKeys(o *telemetry.OTLPConfig) {
	if o == nil {
		return
	}
	o.ExtraKeysHCL = nil
	targets := []*telemetry.TargetConfig{&o.TargetConfig}
	if o.Retry != nil {
		o.Retry.ExtraKeysHCL = nil
	}
	if o.Metrics != nil {
		o.Metrics.ExtraKeysHCL = nil
		targets = append(targets, &o.Metrics.TargetConfig)
	}
	if o.Traces != nil {
		o.Traces.ExtraKeysHCL = nil
		targets = append(targets, &o.Traces.TargetConfig)
	}
	if o.Logs != nil {
		o.Logs.ExtraKeysHCL = nil
		targets = append(targets, &o.Logs.TargetConfig)
	}
	for _, tc := range targets {
		if tc.TLS != nil {
			tc.TLS.ExtraKeysHCL = nil
		}
	}
}

func TestConfig_Parse_OTLP_UnknownKeys(t *testing.T) {
	ci.Parallel(t)

	for name, hcl := range map[string]string{
		"otlp":       `telemetry { otlp { endpont = "x:1" } }`,
		"tls":        `telemetry { otlp { tls { ca = "x" } } }`,
		"retry":      `telemetry { otlp { retry { max = "1s" } } }`,
		"metrics":    `telemetry { otlp { metrics { intervall = "1s" } } }`,
		"traces":     `telemetry { otlp { traces { ratio = 0.5 } } }`,
		"logs":       `telemetry { otlp { logs { lvl = "info" } } }`,
		"signal tls": `telemetry { otlp { traces { tls { ca = "x" } } } }`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseConfigString(t, "agent.hcl", hcl)
			must.ErrorContains(t, err, "unexpected keys")
		})
	}
}

// Two configuration files merge key by key; insecure and tls of a later file
// replace each other.
func TestConfig_Merge_OTLP(t *testing.T) {
	ci.Parallel(t)

	first, err := parseConfigString(t, "a.hcl", `telemetry {
  otlp {
    endpoint = "central.example.com:4317"
    headers  = { "authorization" = "Bearer a" }
    tls { ca_file = "/ca.pem" }
    traces { sample_ratio = 0.5 }
  }
}`)
	must.NoError(t, err)
	second, err := parseConfigString(t, "b.hcl", `telemetry {
  otlp {
    endpoint = "127.0.0.1:4317"
    insecure = true
    traces { trust_http_parent = true }
  }
}`)
	must.NoError(t, err)

	merged := DefaultConfig().Merge(first).Merge(second)
	o := merged.Telemetry.OTLP
	must.Eq(t, "127.0.0.1:4317", o.Endpoint)
	must.True(t, *o.Insecure)
	must.Nil(t, o.TLS)
	must.Eq(t, map[string]string{"authorization": "Bearer a"}, o.Headers)
	must.Eq(t, 0.5, *o.Traces.SampleRatio)
	must.True(t, *o.Traces.TrustHTTPParent)
	must.Eq(t, "/ca.pem", first.Telemetry.OTLP.TLS.CAFile, must.Sprint("the files are not changed"))

	// the merged block is valid
	_, err = telemetry.Resolve(o)
	must.NoError(t, err)

	// a copy of the configuration does not share the block
	cp := merged.Copy()
	cp.Telemetry.OTLP.Headers["authorization"] = "x"
	must.Eq(t, "Bearer a", merged.Telemetry.OTLP.Headers["authorization"])
}

func TestValidateNorcaBlocks_OTLP(t *testing.T) {
	ci.Parallel(t)

	c := DefaultConfig()
	must.NoError(t, validateNorcaBlocks(c))

	c.Telemetry.OTLP = &telemetry.OTLPConfig{}
	must.NoError(t, validateNorcaBlocks(c))

	c.Telemetry.OTLP = &telemetry.OTLPConfig{TargetConfig: telemetry.TargetConfig{
		Endpoint: "10.0.0.1:4317",
		Insecure: pointer.Of(true),
		Headers:  map[string]string{"authorization": "Bearer x"},
	}}
	must.ErrorContains(t, validateNorcaBlocks(c), "telemetry.otlp.metrics.headers")

	// the files are not read here: norca config validate may run elsewhere
	c.Telemetry.OTLP = &telemetry.OTLPConfig{TargetConfig: telemetry.TargetConfig{
		TLS: &telemetry.TLSConfig{CAFile: "/nonexistent/ca.pem"},
	}}
	must.NoError(t, validateNorcaBlocks(c))
}

// The event export of a server needs the events of the system bus, with
// its topics among the published ones; on a client it is only warned about.
func TestValidateNorcaBlocks_EventExport(t *testing.T) {
	ci.Parallel(t)

	exportConfig := func(topics ...string) *Config {
		c := DefaultConfig()
		c.Server.Enabled = true
		c.Telemetry.OTLP = &telemetry.OTLPConfig{Logs: &telemetry.LogsConfig{
			Enabled: pointer.Of(true),
			Events:  &telemetry.EventsConfig{Enabled: pointer.Of(true), Topics: topics},
		}}
		return c
	}

	must.NoError(t, validateNorcaBlocks(exportConfig()), must.Sprint("the bus publishes every topic by default"))

	c := exportConfig()
	c.Server.Bus = &BusConfig{Enabled: pointer.Of(false)}
	must.ErrorContains(t, validateNorcaBlocks(c), "telemetry.otlp.logs.events: the export reads the event stream of the system bus")

	c = exportConfig()
	c.Server.Bus = &BusConfig{Events: &BusEventsConfig{Enabled: pointer.Of(false)}}
	must.ErrorContains(t, validateNorcaBlocks(c), "needs server.bus and its events enabled")

	c = exportConfig("Job", "Secret")
	c.Server.Bus = &BusConfig{Events: &BusEventsConfig{Topics: []string{"Job", "Allocation"}}}
	must.ErrorContains(t, validateNorcaBlocks(c), `the topics ["Secret"] are not in server.bus.events.topics`)

	c = exportConfig("Job", "Allocation")
	c.Server.Bus = &BusConfig{Events: &BusEventsConfig{Topics: []string{"Job", "Allocation"}}}
	must.NoError(t, validateNorcaBlocks(c))

	// a client has no event stream
	c = exportConfig()
	c.Server.Enabled = false
	c.Server.Bus = &BusConfig{Enabled: pointer.Of(false)}
	must.NoError(t, validateNorcaBlocks(c))

	// the logs signal is needed in any case
	c = exportConfig()
	c.Telemetry.OTLP.Logs.Enabled = pointer.Of(false)
	must.ErrorContains(t, validateNorcaBlocks(c), "needs logs.enabled = true")
}

func TestTelemetry_DevConfig_NoOTLP(t *testing.T) {
	ci.Parallel(t)

	must.Nil(t, DevConfig(nil).Telemetry.OTLP)
	must.Nil(t, DefaultConfig().Telemetry.OTLP)
}

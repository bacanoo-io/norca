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

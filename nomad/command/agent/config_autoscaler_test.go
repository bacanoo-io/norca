// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/autoscaler"
	"github.com/shoenig/test/must"
)

func TestConfig_Autoscaler_Parse(t *testing.T) {
	ci.Parallel(t)

	hcl := `
server {
  enabled = true
  autoscaler {
    enabled                     = true
    default_engine              = "builtin"
    min_eval_interval           = "15s"
    default_evaluation_interval = "30s"
    default_cooldown            = "2m"
    plugin_dir                  = "/opt/nomad/autoscaler-plugins"
    eval_workers                = 4
    rate_limit                  = 5
    rate_limit_burst            = 20
    metrics_window              = "10m"
    client_metrics_interval     = "15s"

    apm "prom" {
      driver = "prometheus"
      config {
        address = "http://prometheus:9090"
      }
    }
  }
}
`
	path := filepath.Join(t.TempDir(), "server.hcl")
	must.NoError(t, os.WriteFile(path, []byte(hcl), 0o600))

	c, err := ParseConfigFile(path)
	must.NoError(t, err)
	must.NotNil(t, c.Server.Autoscaler)

	cfg, err := c.Server.Autoscaler.toServerConfig()
	must.NoError(t, err)
	must.True(t, cfg.Enabled)
	must.Eq(t, "builtin", cfg.DefaultEngine)
	must.Eq(t, 15*time.Second, cfg.MinEvalInterval)
	must.Eq(t, 30*time.Second, cfg.DefaultEvaluationInterval)
	must.Eq(t, 2*time.Minute, cfg.DefaultCooldown)
	must.Eq(t, "/opt/nomad/autoscaler-plugins", cfg.PluginDir)
	must.Eq(t, 4, cfg.EvalWorkers)
	must.Eq(t, 5.0, cfg.RateLimit)
	must.Eq(t, 20, cfg.RateLimitBurst)
	must.Eq(t, 10*time.Minute, cfg.MetricsWindow)
	must.Eq(t, 15*time.Second, cfg.ClientMetricsInterval)
	must.Len(t, 1, cfg.APMs)
	must.Eq(t, &autoscaler.PluginConfig{
		Name:   "prom",
		Driver: "prometheus",
		Config: map[string]string{"address": "http://prometheus:9090"},
	}, cfg.APMs[0])

	// The block survives merging with the default configuration.
	merged := DefaultConfig().Merge(c)
	cfg2, err := merged.Server.Autoscaler.toServerConfig()
	must.NoError(t, err)
	must.Eq(t, cfg, cfg2)
}

func TestConfig_Autoscaler_Defaults(t *testing.T) {
	ci.Parallel(t)

	var a *AutoscalerConfig
	cfg, err := a.toServerConfig()
	must.NoError(t, err)
	must.Eq(t, autoscaler.DefaultConfig(), cfg)
	must.False(t, cfg.Enabled)
	must.Eq(t, "external", cfg.DefaultEngine)
}

func TestConfig_Autoscaler_Invalid(t *testing.T) {
	ci.Parallel(t)

	cases := []*AutoscalerConfig{
		{DefaultEngine: "other"},
		{MinEvalInterval: "soon"},
		{APMs: []*AutoscalerPluginConfig{{Name: "dd", Driver: "datadog"}}},
	}
	for _, c := range cases {
		_, err := c.toServerConfig()
		must.Error(t, err)
	}
}

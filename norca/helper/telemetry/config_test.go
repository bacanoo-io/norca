// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/shoenig/test/must"
)

func fullConfig() *OTLPConfig {
	target := func(endpoint string) TargetConfig {
		return TargetConfig{
			Endpoint:    endpoint,
			Insecure:    pointer.Of(false),
			Headers:     map[string]string{"authorization": "Bearer " + endpoint},
			HeadersFile: "/h-" + endpoint,
			TLS:         &TLSConfig{CAFile: "/ca-" + endpoint},
		}
	}
	return &OTLPConfig{
		TargetConfig:       target("a:1"),
		Compression:        "gzip",
		Timeout:            "1s",
		Retry:              &RetryConfig{Enabled: pointer.Of(true), InitialInterval: "1s", MaxInterval: "2s", MaxElapsed: "3s"},
		ResourceAttributes: map[string]string{"k": "v"},
		Metrics:            &MetricsConfig{TargetConfig: target("m:1"), Enabled: pointer.Of(true), Interval: "1s", Temporality: "delta", Histograms: "explicit"},
		Traces: &TracesConfig{
			TargetConfig:    target("t:1"),
			Enabled:         pointer.Of(true),
			SampleRatio:     pointer.Of(0.5),
			TrustHTTPParent: pointer.Of(true),
			ExcludeRPC:      []string{"A.B"},
			MaxQueueSize:    10,
			MaxExportBatch:  5,
		},
		Logs: &LogsConfig{TargetConfig: target("l:1"), Enabled: pointer.Of(true), Level: "warn"},
	}
}

func TestOTLPConfig_Copy(t *testing.T) {
	ci.Parallel(t)

	must.Nil(t, (*OTLPConfig)(nil).Copy())

	a := fullConfig()
	b := a.Copy()
	must.Eq(t, a, b)

	// every reference is its own
	b.Headers["authorization"] = "x"
	*b.Insecure = true
	b.TLS.CAFile = "x"
	*b.Retry.Enabled = false
	b.ResourceAttributes["k"] = "x"
	b.Metrics.Headers["authorization"] = "x"
	*b.Metrics.Enabled = false
	b.Traces.TLS.CAFile = "x"
	*b.Traces.SampleRatio = 1
	b.Traces.ExcludeRPC[0] = "x"
	*b.Logs.Enabled = false
	must.Eq(t, fullConfig(), a)
}

func TestOTLPConfig_Merge(t *testing.T) {
	ci.Parallel(t)

	t.Run("nil sides", func(t *testing.T) {
		must.Eq(t, fullConfig(), (*OTLPConfig)(nil).Merge(fullConfig()))
		must.Eq(t, fullConfig(), fullConfig().Merge(nil))
		must.Nil(t, (*OTLPConfig)(nil).Merge(nil))
	})

	t.Run("empty overlay keeps everything", func(t *testing.T) {
		must.Eq(t, fullConfig(), fullConfig().Merge(&OTLPConfig{}))
	})

	t.Run("overlay replaces key by key", func(t *testing.T) {
		a := fullConfig()
		got := a.Merge(&OTLPConfig{
			TargetConfig:       TargetConfig{Endpoint: "b:2", Headers: map[string]string{"x": "y"}},
			Compression:        "none",
			Timeout:            "9s",
			Retry:              &RetryConfig{MaxElapsed: "9s"},
			ResourceAttributes: map[string]string{"n": "m"},
			Metrics:            &MetricsConfig{Interval: "9s"},
			Traces:             &TracesConfig{SampleRatio: pointer.Of(0.0), ExcludeRPC: []string{}},
			Logs:               &LogsConfig{Enabled: pointer.Of(false)},
		})
		must.Eq(t, "b:2", got.Endpoint)
		must.Eq(t, map[string]string{"x": "y"}, got.Headers)
		must.Eq(t, "/h-a:1", got.HeadersFile)
		must.Eq(t, "/ca-a:1", got.TLS.CAFile)
		must.Eq(t, "none", got.Compression)
		must.Eq(t, "9s", got.Timeout)
		must.Eq(t, &RetryConfig{Enabled: pointer.Of(true), InitialInterval: "1s", MaxInterval: "2s", MaxElapsed: "9s"}, got.Retry)
		must.Eq(t, map[string]string{"n": "m"}, got.ResourceAttributes)
		must.Eq(t, "9s", got.Metrics.Interval)
		must.Eq(t, "delta", got.Metrics.Temporality)
		must.Eq(t, 0.0, *got.Traces.SampleRatio)
		must.SliceEmpty(t, got.Traces.ExcludeRPC)
		must.Eq(t, 10, got.Traces.MaxQueueSize)
		must.False(t, *got.Logs.Enabled)
		must.Eq(t, "warn", got.Logs.Level)

		must.Eq(t, fullConfig(), a, must.Sprint("the base is not changed"))
	})

	t.Run("insecure and tls are one key", func(t *testing.T) {
		got := fullConfig().Merge(&OTLPConfig{TargetConfig: TargetConfig{Insecure: pointer.Of(true)}})
		must.True(t, *got.Insecure)
		must.Nil(t, got.TLS)

		got = got.Merge(&OTLPConfig{TargetConfig: TargetConfig{TLS: &TLSConfig{ServerName: "s"}}})
		must.Nil(t, got.Insecure)
		must.Eq(t, &TLSConfig{ServerName: "s"}, got.TLS)
	})
}

func TestOTLPConfig_Redact(t *testing.T) {
	ci.Parallel(t)

	(*OTLPConfig)(nil).Redact()

	c := fullConfig()
	c.Metrics.Headers = nil
	c.Redact()
	must.Eq(t, map[string]string{"authorization": redactedValue}, c.Headers)
	must.Nil(t, c.Metrics.Headers)
	must.Eq(t, map[string]string{"authorization": redactedValue}, c.Traces.Headers)
	must.Eq(t, map[string]string{"authorization": redactedValue}, c.Logs.Headers)
	must.Eq(t, "/h-a:1", c.HeadersFile, must.Sprint("the path is not a secret"))
}

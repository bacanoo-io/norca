// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"testing"

	metrics "github.com/hashicorp/go-metrics"
	gmprometheus "github.com/hashicorp/go-metrics/prometheus"
	promclient "github.com/prometheus/client_golang/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// benchSinks are the sinks compared per call: the OTel bridge bound to a
// provider with a manual reader, and the go-metrics Prometheus sink with the
// agent's options, on its own registry.
func benchSinks(b *testing.B) map[string]metrics.MetricSink {
	b.Helper()
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewManualReader()),
		sdkmetric.WithCardinalityLimit(cardinalityLimit),
	)
	b.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	prom, err := gmprometheus.NewPrometheusSinkFrom(gmprometheus.PrometheusOpts{
		Expiration: DefaultSeriesExpiry,
		Registerer: promclient.NewRegistry(),
	})
	if err != nil {
		b.Fatal(err)
	}
	return map[string]metrics.MetricSink{
		"sink=otel":       NewOTelSink(mp, OTelSinkOptions{}),
		"sink=prometheus": prom,
	}
}

// benchKey and benchLabels are a typical allocation metric as the sinks get
// it: service name in front, the host label last.
var (
	benchKey    = []string{"norca", "client", "allocs", "cpu", "total_percent"}
	benchLabels = []metrics.Label{
		{Name: "alloc_id", Value: "8f0c5a8e-4f5d-4c0a-9d3e-1b2c3d4e5f60"},
		{Name: "task", Value: "web"},
		{Name: "namespace", Value: "default"},
		{Name: "host", Value: "node-1"},
	}
	benchTimer = []string{"norca", "server", "rpc", "request_latency"}
)

func BenchmarkOTelSink(b *testing.B) {
	for _, call := range []struct {
		name string
		do   func(metrics.MetricSink)
	}{
		{"call=IncrCounterWithLabels", func(s metrics.MetricSink) { s.IncrCounterWithLabels(benchKey, 1, benchLabels) }},
		{"call=SetGaugeWithLabels", func(s metrics.MetricSink) { s.SetGaugeWithLabels(benchKey, 42, benchLabels) }},
		{"call=AddSampleWithLabels", func(s metrics.MetricSink) { s.AddSampleWithLabels(benchTimer, 3.5, benchLabels[3:]) }},
	} {
		for name, sink := range benchSinks(b) {
			b.Run(call.name+"/"+name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					call.do(sink)
				}
			})
			b.Run(call.name+"/"+name+"/parallel", func(b *testing.B) {
				b.ReportAllocs()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						call.do(sink)
					}
				})
			})
		}
	}
}

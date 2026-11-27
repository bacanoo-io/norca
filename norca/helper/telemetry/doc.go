// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package telemetry is the OpenTelemetry part of the agent's telemetry: the
// otlp block of the configuration, the resource, the OTLP/gRPC exporters,
// the providers with their lifecycle, the error reporting of the export,
// the names of the agent's metrics and the go-metrics sink that bridges
// them to the OTLP export. The go-metrics setup (in-memory and Prometheus
// sinks, the fanout) stays in command/agent.
//
// The trace instrumentation (Tracing) is here too: the sampler with the
// always-sampled operations and the exclusions, the W3C traceparent that
// RPC arguments carry, the server span wrapper of the msgpack RPC codec,
// the HTTP API handler, and the trace fields of the logs (LogWith); the
// names and attributes of the scheduling and allocation spans (spans.go);
// and on the clients the phase tracking of the runners (SpanTracker), the
// client spans of the RPCs made within a phase (TraceRPCs) and the gRPC
// instrumentation of the driver plugin clients (PluginDialOptions).
//
// The agent configuration is the only source of the settings: every option
// that the exporters or the SDK would read from an OTEL_* environment
// variable is set explicitly, and the resource is pinned on the exported
// data.
package telemetry

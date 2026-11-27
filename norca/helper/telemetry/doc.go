// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package telemetry is the OpenTelemetry part of the agent's telemetry: the
// otlp block of the configuration, the resource, the OTLP/gRPC exporters,
// the providers with their lifecycle, and the error reporting of the
// export. The go-metrics setup (in-memory and Prometheus sinks) stays in
// command/agent.
//
// The agent configuration is the only source of the settings: every option
// that the exporters or the SDK would read from an OTEL_* environment
// variable is set explicitly, and the resource is pinned on the exported
// data.
package telemetry

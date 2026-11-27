// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestSampler(t *testing.T) {
	ci.Parallel(t)

	sampler := NewSampler(sdktrace.NeverSample())
	decide := func(ctx context.Context, name string) sdktrace.SamplingDecision {
		return sampler.ShouldSample(sdktrace.SamplingParameters{
			ParentContext: ctx, TraceID: testTraceID, Name: name,
		}).Decision
	}
	bg := context.Background()

	// new roots: the important operations always, the rest at the ratio
	must.Eq(t, sdktrace.RecordAndSample, decide(bg, "Job.Register"))
	must.Eq(t, sdktrace.RecordAndSample, decide(bg, "Operator.RaftRemovePeerByID"))
	must.Eq(t, sdktrace.RecordAndSample, decide(bg, "Node.UpdateDrain"))
	must.Eq(t, sdktrace.RecordAndSample, decide(bg, SpanAllocStart))
	must.Eq(t, sdktrace.RecordAndSample, decide(withAlwaysSample(bg), "PUT /v1/job/"))
	must.Eq(t, sdktrace.Drop, decide(bg, "Job.GetJob"))
	must.Eq(t, sdktrace.Drop, decide(bg, "PUT /v1/job/"))

	// a parent decides, also for the important operations
	sampledParent := trace.ContextWithRemoteSpanContext(bg, testSpanContext(true))
	unsampledParent := trace.ContextWithRemoteSpanContext(bg, testSpanContext(false))
	must.Eq(t, sdktrace.RecordAndSample, decide(sampledParent, "Job.GetJob"))
	must.Eq(t, sdktrace.Drop, decide(unsampledParent, "Job.Register"))

	must.StrContains(t, sampler.Description(), "NorcaRoot")
	must.True(t, AlwaysSampledRPC("Job.Scale"))
	must.False(t, AlwaysSampledRPC("Job.List"))
}

// Every always-sampled RPC is a real method name (Service.Method) and none
// is excluded at the same time.
func TestAlwaysSampledRPC_Form(t *testing.T) {
	ci.Parallel(t)

	excluded := setOf(excludedRPC...)
	for m := range alwaysSampledRPC {
		service, method, ok := strings.Cut(m, ".")
		must.True(t, ok && service != "" && method != "", must.Sprint(m))
		_, ok = excluded[m]
		must.False(t, ok, must.Sprint(m))
	}
}

func TestAlwaysSampleHTTP(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		method, pattern, path string
		expected              bool
	}{
		{"PUT", "/v1/jobs", "/v1/jobs", true},
		{"POST", "/v1/job/", "/v1/job/web/scale", true},
		{"DELETE", "/v1/job/", "/v1/job/web", true},
		{"GET", "/v1/job/", "/v1/job/web", false},
		{"GET", "/v1/jobs", "/v1/jobs", false},
		{"POST", "/v1/node/", "/v1/node/abc/drain", true},
		{"GET", "/v1/operator/raft/", "/v1/operator/raft/configuration", true},
		{"GET", "/v1/operator/snapshot", "/v1/operator/snapshot", true},
		{"GET", "/v1/operator/keyring/", "/v1/operator/keyring/keys", false},
		{"PUT", "/v1/object/", "/v1/object/norca.io/NodeGroup/web", true},
		{"PUT", "/v1/object/", "/v1/object/norca.io/NodeGroup/web/scale", true},
		{"PUT", "/v1/object/", "/v1/object/norca.io/NodeGroup/web/status", false},
		{"PUT", "/v1/object/", "/v1/object/norca.io/NodeGroup/web/finalizers", false},
		{"POST", "/v1/object/", "/v1/object/norca.io/NodeGroup/web/events", false},
		{"PUT", "/v1/var/", "/v1/var/app/config", false},
		{"PUT", "", "/nowhere", false},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			must.Eq(t, tc.expected, alwaysSampleHTTP(tc.method, tc.pattern, tc.path))
		})
	}
}

func TestExcludeHTTP(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		url, pattern string
		expected     bool
	}{
		{"/v1/agent/health", "/v1/agent/health", true},
		{"/v1/metrics?format=prometheus", "/v1/metrics", true},
		{"/v1/metrics/history?metric=x", "/v1/metrics/history", true},
		{"/v1/event/stream", "/v1/event/stream", true},
		{"/v1/agent/monitor", "/v1/agent/monitor", true},
		{"/ui/jobs", "/ui/", true},
		{"/v1/client/fs/logs/abc?task=web", "/v1/client/fs/", true},
		{"/v1/client/fs/stream/abc", "/v1/client/fs/", true},
		{"/v1/client/fs/ls/abc", "/v1/client/fs/", false},
		{"/v1/client/allocation/abc/exec", "/v1/client/allocation/", true},
		{"/v1/client/allocation/abc/stats", "/v1/client/allocation/", false},
		{"/v1/jobs?index=12", "/v1/jobs", true},
		{"/v1/jobs?index=0", "/v1/jobs", false},
		{"/v1/jobs?prefix=web", "/v1/jobs", false},
		{"/v1/job/web", "/v1/job/", false},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			r := httptest.NewRequest("GET", tc.url, nil)
			must.Eq(t, tc.expected, excludeHTTP(r, tc.pattern))
		})
	}
}

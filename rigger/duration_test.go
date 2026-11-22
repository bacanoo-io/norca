// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/hashicorp/hcl/v2"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/host"
)

// The bound of an evaluation in these tests, and a host slower than it.
const (
	shortEvaluation = 200 * time.Millisecond
	slowHost        = 400 * time.Millisecond
)

type slowJobs struct{}

func (slowJobs) DecodeJob(context.Context, host.JobSpec) (host.Job, hcl.Diagnostics) {
	time.Sleep(slowHost)

	return host.Job{Spec: []byte(`{}`)}, nil
}

type slowReleases struct{ *fakeCluster }

func (s slowReleases) Current(ctx context.Context, ref host.ReleaseRef) (host.Revision, error) {
	select {
	case <-ctx.Done():
		return host.Revision{}, ctx.Err()
	case <-time.After(slowHost):
		return s.fakeCluster.Current(ctx, ref)
	}
}

func shortEngine(t *testing.T, opts ...rigger.Option) *rigger.Engine {
	t.Helper()

	limits := rigger.DefaultLimits()
	limits.Duration = shortEvaluation

	e, err := rigger.New(append([]rigger.Option{rigger.WithLimits(limits), rigger.WithLogger(slog.New(slog.DiscardHandler))}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}

	return e
}

// The bound of the time is that of the evaluation: the calls to the cluster
// that compare the releases have the context of the caller.
func TestPlanStack_DurationLeavesTheCluster(t *testing.T) {
	t.Parallel()

	f := slowReleases{newFakeCluster()}
	e := shortEngine(t, rigger.WithReleases(f), rigger.WithSecrets(f))

	if got := actions(planOf(t, e, stackFiles(twoReleases), nil)); got != bothInstalled {
		t.Errorf("plan = %s", got)
	}
}

// An install is bounded as an evaluation and a stack are.
func TestPlanInstall_Duration(t *testing.T) {
	t.Parallel()

	f := newFakeCluster()
	e := shortEngine(t, rigger.WithReleases(f), rigger.WithJobDecoder(slowJobs{}))

	m, diags := e.LoadModule(t.Context(), fstest.MapFS{
		metadataFile:  {Data: []byte("module {\n  name    = \"slow\"\n  version = \"0.1.0\"\n}\n")},
		"a.norca.hcl": {Data: []byte("job \"a\" {\n}\n")},
		"b.norca.hcl": {Data: []byte("job \"b\" {\n}\n")},
	})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	_, diags = e.PlanInstall(t.Context(), m, rigger.Install{Release: "slow", Namespace: testNamespace, Region: "eu"}, host.Settings{})
	if !strings.Contains(diags.Error(), "Evaluation stopped") {
		t.Errorf("diagnostics = %q, want the evaluation stopped", diags.Error())
	}
}

// The time is looked at between the locals and the copies of a resource
// too, not only between the resources.
func TestEvaluate_Stops(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files fstest.MapFS
	}{
		{name: "locals", files: fstest.MapFS{"locals.hcl": {Data: []byte("locals {\n  a = 1\n  b = local.a + 1\n}\n")}}},
		{name: "copies", files: fstest.MapFS{"a.norca.hcl": {Data: []byte("job \"a\" {\n  count = 4\n}\n")}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tt.files[metadataFile] = &fstest.MapFile{Data: []byte("module {\n  name    = \"m\"\n  version = \"0.1.0\"\n}\n")}

			e := shortEngine(t, rigger.WithJobDecoder(slowJobs{}))

			m, diags := e.LoadModule(t.Context(), tt.files)
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			if tt.name == "locals" {
				cancel()
			}

			_, diags = e.Evaluate(ctx, m, rigger.Install{Release: "m", Namespace: testNamespace, Region: "eu"})
			if !strings.Contains(diags.Error(), "Evaluation stopped") {
				t.Errorf("diagnostics = %q, want the evaluation stopped", diags.Error())
			}
		})
	}
}

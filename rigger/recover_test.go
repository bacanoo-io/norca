// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"bytes"
	"context"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hashicorp/hcl/v2"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/host"
	"github.com/bacanoo-io/norca/rigger/source"
)

// faultText is the panic of the faulty hosts: the text of a panic may hold
// a sensitive value, so it is neither returned nor logged.
const faultText = "fault-with-a-sensitive-value"

type faultyFS struct{}

func (faultyFS) Open(string) (fs.File, error) { panic(faultText) }

type faultyFetcher struct{}

func (faultyFetcher) Versions(context.Context, source.Request) ([]source.Available, error) {
	panic(faultText)
}

func (faultyFetcher) Fetch(context.Context, source.Request) (source.Fetched, error) {
	panic(faultText)
}

type faultyJobs struct{}

func (faultyJobs) DecodeJob(context.Context, host.JobSpec) (host.Job, hcl.Diagnostics) {
	panic(faultText)
}

type faultyReleases struct{ *fakeCluster }

func (faultyReleases) Current(context.Context, host.ReleaseRef) (host.Revision, error) {
	panic(faultText)
}

func (faultyReleases) Apply(context.Context, host.ApplyRequest) (host.Revision, error) {
	panic(faultText)
}

func (faultyReleases) Owned(context.Context, string) ([]host.Revision, error) {
	panic(faultText)
}

// A panic under the engine (a decoder, a host, the parser of a file someone
// else wrote) is an error of the operation: a server embeds the engine, and
// a panic would end its process.
func TestEngine_RecoversPanic(t *testing.T) {
	t.Parallel()

	good := newFakeCluster()
	plan := planOf(t, stackEngine(t, good), stackFiles(twoReleases), nil)

	st, diags := stackEngine(t, good).LoadStack(t.Context(), stackFiles(twoReleases), "s")
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	files := stackFiles("")
	delete(files, "s/modules/app/variables.hcl")
	delete(files, "s/modules/app/app.var.hcl")
	files["s/modules/app/j.norca.hcl"] = &fstest.MapFile{Data: []byte("job \"j\" {\n}\n")}

	job, diags := engine(t).LoadModuleAt(t.Context(), files, "s/modules/app")
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	for _, tt := range append(readingOperations(t, job), clusterOperations(t, job, st, plan)...) {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			checkRecovered(t, tt.run)
		})
	}
}

// engineOperation is an operation of the engine for the test of the panics.
type engineOperation struct {
	name string
	run  func(e *rigger.Engine) error
}

// readingOperations are the operations that read what others wrote.
func readingOperations(t *testing.T, job *rigger.Module) []engineOperation {
	t.Helper()

	in := rigger.Install{Release: "j", Namespace: testNamespace, Region: "eu"}
	req := source.Request{Source: "acme/j", Kind: source.KindGit, Version: "2.0.0"}

	return []engineOperation{
		{name: "load module", run: func(e *rigger.Engine) error {
			_, d := e.LoadModule(t.Context(), faultyFS{})

			return d
		}},
		{name: "load stack", run: func(e *rigger.Engine) error {
			_, d := e.LoadStack(t.Context(), faultyFS{}, ".")

			return d
		}},
		{name: "read stack", run: func(e *rigger.Engine) error {
			_, d := e.ReadStack(t.Context(), faultyFS{}, ".")

			return d
		}},
		{name: "sync stack", run: func(e *rigger.Engine) error {
			_, _, d := e.SyncStack(t.Context(), faultyFS{}, ".", false)

			return d
		}},
		{name: "module digest", run: func(e *rigger.Engine) error {
			_, err := e.ModuleDigest(t.Context(), faultyFS{}, ".")

			return err
		}},
		{name: "fetch module", run: func(e *rigger.Engine) error {
			_, d := e.FetchModule(t.Context(), req, nil)

			return d
		}},
		{name: "fetch stack", run: func(e *rigger.Engine) error {
			_, d := e.FetchStack(t.Context(), "acme/data", "")

			return d
		}},
		{name: "evaluate", run: func(e *rigger.Engine) error {
			_, d := e.Evaluate(t.Context(), job, in)

			return d
		}},
	}
}

// clusterOperations are the operations that use the hosts of the cluster.
func clusterOperations(t *testing.T, job *rigger.Module, st *rigger.Stack, plan *rigger.StackPlan) []engineOperation {
	t.Helper()

	in := rigger.Install{Release: "j", Namespace: testNamespace, Region: "eu"}

	return []engineOperation{
		{name: "plan install", run: func(e *rigger.Engine) error {
			_, d := e.PlanInstall(t.Context(), job, in, host.Settings{})

			return d
		}},
		{name: "plan stack", run: func(e *rigger.Engine) error {
			_, d := e.PlanStack(t.Context(), st, nil)

			return d
		}},
		{name: "apply stack", run: func(e *rigger.Engine) error {
			_, d := e.ApplyStack(t.Context(), plan, rigger.ApplyOptions{})

			return d
		}},
		{name: "apply install", run: func(e *rigger.Engine) error {
			_, err := e.ApplyInstall(t.Context(), &plan.Releases[0], rigger.ApplyOptions{})

			return err
		}},
		{name: "stack status", run: func(e *rigger.Engine) error {
			_, err := e.StackStatus(t.Context(), st)

			return err
		}},
		{name: "destroy stack", run: func(e *rigger.Engine) error {
			_, err := e.DestroyStack(t.Context(), st)

			return err
		}},
	}
}

// checkRecovered runs an operation on an engine of faulty hosts.
func checkRecovered(t *testing.T, run func(e *rigger.Engine) error) {
	t.Helper()

	var log bytes.Buffer

	f := faultyReleases{newFakeCluster()}

	e, err := rigger.New(rigger.WithReleases(f), rigger.WithSecrets(f), rigger.WithFetcher(faultyFetcher{}),
		rigger.WithJobDecoder(faultyJobs{}), rigger.WithLogger(slog.New(slog.NewTextHandler(&log, nil))))
	if err != nil {
		t.Fatal(err)
	}

	noPanic(t, func() { err = run(e) })

	if err == nil || !strings.Contains(err.Error(), rigger.ErrInternal.Error()) {
		t.Errorf("error = %v, want %v", err, rigger.ErrInternal)
	}

	if !strings.Contains(log.String(), "recovered a panic") || !strings.Contains(log.String(), "goroutine") {
		t.Errorf("log = %q, want the stack of the panic", log.String())
	}

	if strings.Contains(log.String(), faultText) || (err != nil && strings.Contains(err.Error(), faultText)) {
		t.Errorf("the text of the panic is shown: %v, %s", err, log.String())
	}
}

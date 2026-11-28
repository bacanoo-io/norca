// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/host"
)

// pipeProcess is a child of the evaluations in this process: a goroutine
// that serves them over two pipes.
type pipeProcess struct {
	*io.PipeReader // its answers
	in             *io.PipeWriter
	done           chan error
}

func (p *pipeProcess) Write(b []byte) (int, error) { return p.in.Write(b) }

func (p *pipeProcess) Close() error {
	_ = p.in.Close()

	return p.PipeReader.Close()
}

func (p *pipeProcess) Wait() error { return <-p.done }

// childStarter starts children that serve the evaluations with serve, and
// counts them.
func childStarter(started *atomic.Int32, serve func(ctx context.Context, r io.Reader, w io.Writer) error) rigger.ProcessStarter {
	return func(ctx context.Context) (rigger.EvaluationProcess, error) {
		started.Add(1)

		reqR, reqW := io.Pipe()
		respR, respW := io.Pipe()
		p := &pipeProcess{PipeReader: respR, in: reqW, done: make(chan error, 1)}

		go func() {
			err := serve(context.WithoutCancel(ctx), reqR, respW)
			_ = respW.CloseWithError(err)
			_ = reqR.Close()

			p.done <- err
		}()

		return p, nil
	}
}

// serveEvaluations is the child of the tests: ServeEvaluations with an
// engine of the defaults.
func serveEvaluations(ctx context.Context, r io.Reader, w io.Writer) error {
	return rigger.ServeEvaluations(ctx, r, w, rigger.WithLogger(slog.New(slog.DiscardHandler)))
}

// childEngine is an engine over f that evaluates its plans in a child.
func childEngine(t *testing.T, f *fakeCluster, started *atomic.Int32, opts ...rigger.Option) *rigger.Engine {
	t.Helper()

	if started == nil {
		started = &atomic.Int32{}
	}

	e, err := rigger.New(append([]rigger.Option{
		rigger.WithReleases(f), rigger.WithSecrets(f), rigger.WithLogger(slog.New(slog.DiscardHandler)),
		rigger.WithChildEvaluation(childStarter(started, serveEvaluations)),
	}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = e.Close() })

	return e
}

// A stack planned and applied with its evaluation in a child is the stack
// of an engine that evaluates in its own process: after the apply, a plan
// of that engine changes nothing, generated values and digests included.
// One child serves all the plans of the engine.
func TestChildEvaluation_StackPlanApply(t *testing.T) {
	t.Parallel()

	f := newFakeCluster()
	started := &atomic.Int32{}
	e := childEngine(t, f, started)
	fsys := stackFiles(twoReleases)

	plan := planOf(t, e, fsys, nil)
	if got := actions(plan); got != bothInstalled {
		t.Fatalf("plan = %s, want db then app installed", got)
	}

	if strings.Join(plan.Generate, ",") != "db" || plan.Secrets[0].Action != "create" {
		t.Errorf("plan generates %v, secrets %+v", plan.Generate, plan.Secrets)
	}

	if _, diags := e.ApplyStack(t.Context(), plan, rigger.ApplyOptions{}); diags.HasErrors() {
		t.Fatal(diags)
	}

	checkApplied(t, f)

	here := stackEngine(t, f)
	if again := planOf(t, here, fsys, nil); actions(again) != bothUnchanged || again.HasChanges() {
		t.Errorf("a plan in this process after the child's apply = %s, generate %v", actions(again), again.Generate)
	}

	bigger := planOf(t, e, fsys, rigger.Values{"size": {Value: ctyNumber(3)}})
	if got := actions(bigger); got != "db:upgrade app:unchanged" {
		t.Errorf("plan with another size in the child = %s", got)
	}

	if n := started.Load(); n != 1 {
		t.Errorf("%d children started for three plans, want one", n)
	}
}

// Sensitive values cross to the child and back marked: the revision records
// none of them, and the secret items hold them.
func TestChildEvaluation_Sensitive(t *testing.T) {
	t.Parallel()

	fsys := sensitiveStack(
		"module \"db\" {\n  source = \"./modules/db\"\n  values = var.db\n}\n",
		"variable \"db\" {\n  sensitive = true\n  default   = { size = 2, password = \"pw\" }\n}\n",
		"secret \"apps/db\" {\n  items = var.db\n}\n",
	)

	f := newFakeCluster()
	e := childEngine(t, f, nil)

	if _, diags := e.ApplyStack(t.Context(), planOf(t, e, fsys, nil), rigger.ApplyOptions{}); diags.HasErrors() {
		t.Fatal(diags)
	}

	db := f.releases[host.ReleaseRef{Namespace: testNamespace, Name: "db"}][0]
	if len(db.Values) != 0 {
		t.Errorf("the revision records %v, want none of a sensitive object", db.Values)
	}

	if size, password := db.Secrets["db"]["size"], db.Secrets["db"]["password"]; size.Reveal() != "2" || password.Reveal() != "pw" {
		t.Errorf("secret items = %q, %q, want 2 and pw", size.Reveal(), password.Reveal())
	}

	if got := f.secrets["apps/db"]["password"]; got.Reveal() != "pw" {
		t.Errorf("the secret holds %q, want pw", got.Reveal())
	}
}

// The parts of a stack and the modules of a registry go to the child with
// the files the engine fetched; the plan is the plan in this process.
func TestChildEvaluation_PartsAndRegistryModules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, stack string
		tree        fstest.MapFS
		expected    string
	}{
		{name: "a part", stack: siteStack, tree: publishedData(), expected: "data-db:install app:install"},
		{
			name: "two releases of a registry module",
			stack: acmeRegistry +
				"\nmodule \"one\" {\n  source  = \"acme/web\"\n  version = \"~> 1.0\"\n}\n" +
				"\nmodule \"two\" {\n  source  = \"acme/web\"\n  version = \"~> 1.0\"\n}\n",
			tree:     webTree(),
			expected: "one:install two:install",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFakeCluster()
			fetch := rigger.WithFetcher(repoFetcher{tree: tt.tree, version: publishedVersion})
			e := childEngine(t, f, nil, fetch)

			files := stackFiles(tt.stack)

			_, lock, diags := e.SyncStack(t.Context(), files, "s", false)
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			files[lockPath] = &fstest.MapFile{Data: lock}

			plan := planOf(t, e, files, nil)
			if got := actions(plan); got != tt.expected {
				t.Fatalf("plan in the child = %s, want %s", got, tt.expected)
			}

			if _, diags := e.ApplyStack(t.Context(), plan, rigger.ApplyOptions{}); diags.HasErrors() {
				t.Fatal(diags)
			}

			here, err := rigger.New(rigger.WithReleases(f), rigger.WithSecrets(f), rigger.WithLogger(slog.New(slog.DiscardHandler)), fetch)
			if err != nil {
				t.Fatal(err)
			}

			if again := planOf(t, here, files, nil); again.HasChanges() {
				t.Errorf("a plan in this process after the child's apply = %s", actions(again))
			}
		})
	}
}

// A diagnostic of an evaluation in the child comes back as the engine in
// this process gives it: its message, its file and its line.
func TestChildEvaluation_Diagnostics(t *testing.T) {
	t.Parallel()

	e := childEngine(t, newFakeCluster(), nil)
	fsys := stackFiles(twoReleases)
	fsys["s/outputs.hcl"] = &fstest.MapFile{Data: []byte("output \"x\" {\n  value = local.missing\n}\n")}

	s, diags := e.LoadStack(t.Context(), fsys, "s")
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	_, diags = e.PlanStack(t.Context(), s, nil)
	if !diags.HasErrors() {
		t.Fatal("a reference to no local planned")
	}

	here := stackEngine(t, newFakeCluster())

	hs, _ := here.LoadStack(t.Context(), fsys, "s")
	_, hereDiags := here.PlanStack(t.Context(), hs, nil)

	if got, want := diags.Error(), hereDiags.Error(); got != want || !strings.Contains(got, "outputs.hcl:2,") {
		t.Errorf("diagnostics of the child = %q, in this process %q", got, want)
	}
}

// A module installed on its own is evaluated in the child too.
func TestChildEvaluation_PlanInstall(t *testing.T) {
	t.Parallel()

	e := childEngine(t, newFakeCluster(), nil)

	m, diags := e.LoadModule(t.Context(), fstest.MapFS{
		metadataFile: {Data: []byte("module {\n  name    = \"app\"\n  version = \"1.0.0\"\n}\n")},
		outputsFile:  {Data: []byte("output \"next_steps\" {\n  value = \"${rigger.release} runs in ${rigger.namespace}.\"\n}\n")},
	})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	rp, diags := e.PlanInstall(t.Context(), m, rigger.Install{Release: appRelease, Namespace: testNamespace, Region: "eu"}, host.Settings{})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if got := rp.NextSteps(); got != "app runs in apps." {
		t.Errorf("NextSteps() = %q", got)
	}
}

// A child that ends before it answers fails the plan with
// ErrEvaluationProcess and how it ended; the next plan starts another.
func TestChildEvaluation_ChildEnds(t *testing.T) {
	t.Parallel()

	started := &atomic.Int32{}
	crash := errors.New("fatal error: stack overflow")

	var calls atomic.Int32

	serve := func(ctx context.Context, r io.Reader, w io.Writer) error {
		if calls.Add(1) == 1 {
			return crash
		}

		return serveEvaluations(ctx, r, w)
	}

	f := newFakeCluster()

	e, err := rigger.New(rigger.WithReleases(f), rigger.WithSecrets(f), rigger.WithLogger(slog.New(slog.DiscardHandler)),
		rigger.WithChildEvaluation(childStarter(started, serve)))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = e.Close() })

	s, diags := e.LoadStack(t.Context(), stackFiles(twoReleases), "s")
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	_, diags = e.PlanStack(t.Context(), s, nil)
	if got := diags.Error(); !strings.Contains(got, "the evaluation process ended") || !strings.Contains(got, "stack overflow") {
		t.Fatalf("diagnostics = %q, want the end of the process", got)
	}

	if _, diags := e.PlanStack(t.Context(), s, nil); diags.HasErrors() {
		t.Fatalf("the plan after it: %v", diags)
	}

	if n := started.Load(); n != 2 {
		t.Errorf("%d children started, want 2", n)
	}
}

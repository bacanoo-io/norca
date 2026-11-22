// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/rigger/host"
	"github.com/bacanoo-io/norca/rigger/oplog"
	"github.com/bacanoo-io/norca/rigger/source"
)

// Fakes of the ports. They do nothing; they show that each port can be
// implemented and let the tests tell the adapters apart.

type fakeCluster struct{ id int }

func (fakeCluster) Info(context.Context) (host.ClusterInfo, error) { return host.ClusterInfo{}, nil }
func (fakeCluster) Namespaces(context.Context) ([]string, error)   { return nil, nil }

type fakeJobs struct{ id int }

func (fakeJobs) DecodeJob(context.Context, host.JobSpec) (host.Job, hcl.Diagnostics) {
	return host.Job{}, nil
}

type fakeReleases struct{ id int }

func (fakeReleases) Current(context.Context, host.ReleaseRef) (host.Revision, error) {
	return host.Revision{}, host.ErrNotFound
}

func (fakeReleases) History(context.Context, host.ReleaseRef) ([]host.Revision, error) {
	return nil, nil
}

func (fakeReleases) List(context.Context, string) ([]host.Revision, error) { return nil, nil }

func (fakeReleases) Owned(context.Context, string) ([]host.Revision, error) { return nil, nil }

func (fakeReleases) Plan(context.Context, host.ApplyRequest) (host.Plan, error) {
	return host.Plan{}, nil
}

func (fakeReleases) Drift(context.Context, host.ReleaseRef) (host.Drift, error) {
	return host.Drift{}, nil
}

func (fakeReleases) Apply(context.Context, host.ApplyRequest) (host.Revision, error) {
	return host.Revision{}, nil
}

func (fakeReleases) Rollback(context.Context, host.ReleaseRef, uint64) (host.Revision, error) {
	return host.Revision{}, nil
}

func (fakeReleases) Uninstall(context.Context, host.ReleaseRef) error { return nil }

type fakeSecrets struct{ id int }

func (fakeSecrets) Ensure(context.Context, host.Secret) (bool, error) { return false, nil }

func (fakeSecrets) Read(context.Context, string) (map[string]host.Sensitive, error) {
	return nil, host.ErrNotFound
}

type fakeStore struct{ id int }

func (fakeStore) Get(context.Context, string) ([]byte, error) { return nil, host.ErrNotFound }
func (fakeStore) Put(context.Context, string, []byte) error   { return nil }
func (fakeStore) Delete(context.Context, string) error        { return nil }

type fakeFetcher struct{ id int }

func (fakeFetcher) Versions(context.Context, source.Request) ([]source.Available, error) {
	return nil, nil
}

func (fakeFetcher) Fetch(context.Context, source.Request) (source.Fetched, error) {
	return source.Fetched{}, nil
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

var (
	_ host.Cluster    = fakeCluster{}
	_ host.JobDecoder = fakeJobs{}
	_ host.Releases   = fakeReleases{}
	_ host.Secrets    = fakeSecrets{}
	_ host.Store      = fakeStore{}
	_ source.Fetcher  = fakeFetcher{}
	_ host.Clock      = fixedClock{}
)

// logsTo reports whether the engine logs to h through the handler of oplog.
func logsTo(e *Engine, h slog.Handler) bool {
	w, ok := e.logger.Handler().(*oplog.Handler)

	return ok && w.Unwrap() == h
}

func TestNew_Defaults(t *testing.T) {
	t.Parallel()

	e, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, ok := e.clock.(host.SystemClock); !ok {
		t.Errorf("clock = %T, want host.SystemClock", e.clock)
	}

	if !logsTo(e, slog.Default().Handler()) {
		t.Error("the logger does not log to the handler of slog.Default() through oplog")
	}

	if e.limits != DefaultLimits() {
		t.Errorf("limits = %+v, want DefaultLimits()", e.limits)
	}

	if e.cluster != nil || e.jobs != nil || e.releases != nil || e.secrets != nil || e.store != nil || e.fetcher != nil {
		t.Error("an adapter is set without its option")
	}
}

func TestNew_Options(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	clock := fixedClock{now: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)}
	limits := Limits{ModuleBytes: 1, FileBytes: 1, Resources: 1, Depth: 1, Modules: 1, Duration: time.Second}

	tests := []struct {
		name  string
		opt   Option
		isSet func(*Engine) bool
	}{
		{name: "cluster", opt: WithCluster(fakeCluster{id: 1}), isSet: func(e *Engine) bool { return e.cluster == fakeCluster{id: 1} }},
		{name: "job decoder", opt: WithJobDecoder(fakeJobs{id: 1}), isSet: func(e *Engine) bool { return e.jobs == fakeJobs{id: 1} }},
		{name: "releases", opt: WithReleases(fakeReleases{id: 1}), isSet: func(e *Engine) bool { return e.releases == fakeReleases{id: 1} }},
		{name: "secrets", opt: WithSecrets(fakeSecrets{id: 1}), isSet: func(e *Engine) bool { return e.secrets == fakeSecrets{id: 1} }},
		{name: "store", opt: WithStore(fakeStore{id: 1}), isSet: func(e *Engine) bool { return e.store == fakeStore{id: 1} }},
		{name: "fetcher", opt: WithFetcher(fakeFetcher{id: 1}), isSet: func(e *Engine) bool { return e.fetcher == fakeFetcher{id: 1} }},
		{name: "clock", opt: WithClock(clock), isSet: func(e *Engine) bool { return e.clock == clock }},
		{name: "logger", opt: WithLogger(logger), isSet: func(e *Engine) bool { return logsTo(e, logger.Handler()) }},
		{name: "limits", opt: WithLimits(limits), isSet: func(e *Engine) bool { return e.limits == limits }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e, err := New(tt.opt)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			if !tt.isSet(e) {
				t.Errorf("the option did not set the %s", tt.name)
			}
		})
	}
}

func TestNew_InvalidOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		opt      Option
		expected string
	}{
		{name: "nil option", opt: nil, expected: "rigger: invalid option: option 0 is nil"},
		{name: "nil cluster", opt: WithCluster(nil), expected: "rigger: invalid option: nil cluster"},
		{name: "nil job decoder", opt: WithJobDecoder(nil), expected: "rigger: invalid option: nil job decoder"},
		{name: "nil releases", opt: WithReleases(nil), expected: "rigger: invalid option: nil releases"},
		{name: "nil secrets", opt: WithSecrets(nil), expected: "rigger: invalid option: nil secrets"},
		{name: "nil store", opt: WithStore(nil), expected: "rigger: invalid option: nil store"},
		{name: "nil fetcher", opt: WithFetcher(nil), expected: "rigger: invalid option: nil fetcher"},
		{name: "nil clock", opt: WithClock(nil), expected: "rigger: invalid option: nil clock"},
		{name: "nil logger", opt: WithLogger(nil), expected: "rigger: invalid option: nil logger"},
		{name: "zero limits", opt: WithLimits(Limits{}), expected: "rigger: invalid option: module bytes 0 is not positive\n" +
			"file bytes 0 is not positive\nresources 0 is not positive\ndepth 0 is not positive\nmodules 0 is not positive\nduration 0s is not positive"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e, err := New(tt.opt)
			if !errors.Is(err, ErrInvalidOption) {
				t.Fatalf("New() error = %v, want ErrInvalidOption", err)
			}

			if err.Error() != tt.expected {
				t.Errorf("New() error = %q, want %q", err, tt.expected)
			}

			if e != nil {
				t.Error("New() returned an engine with an error")
			}
		})
	}
}

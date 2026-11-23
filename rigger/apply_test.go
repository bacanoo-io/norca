// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"testing/fstest"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/host"
)

// otherStack is the stack that owns the releases the tests find taken.
const otherStack = "apps/other"

var errOwned = errors.New("409: the release is of another stack")

// ownedCluster is a cluster that refuses, as the server does, the apply of
// a release another stack owns unless the request adopts it.
type ownedCluster struct{ *fakeCluster }

func (c ownedCluster) Apply(ctx context.Context, req host.ApplyRequest) (host.Revision, error) {
	if cur, err := c.Current(ctx, req.Release); err == nil && cur.Owner != req.Owner && !req.Adopt {
		return host.Revision{}, errOwned
	}

	return c.fakeCluster.Apply(ctx, req)
}

// ownedEngine is an engine over a cluster that keeps the owners.
func ownedEngine(t *testing.T, f *fakeCluster) *rigger.Engine {
	t.Helper()

	c := ownedCluster{f}

	e, err := rigger.New(rigger.WithReleases(c), rigger.WithSecrets(c), rigger.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}

	return e
}

// Adopt takes over what the plan showed as taken over, and no more: a
// release that became another stack's between the plan and the apply is
// refused by the server as without adopt, not taken unseen.
func TestApplyStack_AdoptOnlyPlanned(t *testing.T) {
	t.Parallel()

	f := newFakeCluster()
	e := ownedEngine(t, f)
	db := host.ReleaseRef{Namespace: testNamespace, Name: "db"}
	app := host.ReleaseRef{Namespace: testNamespace, Name: "app"}

	// The plan sees db as of another stack, and no app.
	if _, err := f.Apply(t.Context(), host.ApplyRequest{Release: db, Owner: otherStack}); err != nil {
		t.Fatal(err)
	}

	plan := planOf(t, e, stackFiles(twoReleases), nil)
	if plan.Releases[0].TakesOver == "" || plan.Releases[1].TakesOver != "" || actions(plan) != "db:upgrade app:install" {
		t.Fatalf("plan = %s (%+v), want db taken over and app installed", actions(plan), plan.Releases)
	}

	// The other stack installs app after the plan.
	if _, err := f.Apply(t.Context(), host.ApplyRequest{Release: app, Owner: otherStack}); err != nil {
		t.Fatal(err)
	}

	out, diags := e.ApplyStack(t.Context(), plan, rigger.ApplyOptions{Adopt: true})
	if !diags.HasErrors() || len(out) != 2 {
		t.Errorf("ApplyStack() = %+v, %v; want app refused", out, diags)
	}

	if got := f.releases[db]; len(got) != 2 || got[1].Owner != "apps/shop" || !got[1].Adopt {
		t.Errorf("db has %d revisions, want the planned take-over applied with adopt", len(got))
	}

	if got := f.releases[app]; len(got) != 1 || got[0].Owner != otherStack {
		t.Errorf("app has %d revisions, the last of %q: taken over without a plan of it", len(got), got[len(got)-1].Owner)
	}
}

// So for a release on its own: the plan of an install that takes nothing
// over is applied without adopt, whatever the options say.
func TestApplyInstall_AdoptOnlyPlanned(t *testing.T) {
	t.Parallel()

	f := newFakeCluster()
	e := ownedEngine(t, f)
	app := host.ReleaseRef{Namespace: testNamespace, Name: "app"}

	m, diags := e.LoadModule(t.Context(), fstest.MapFS{metadataFile: {Data: []byte("module {\n  name    = \"app\"\n  version = \"1.0.0\"\n}\n")}})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	rp, diags := e.PlanInstall(t.Context(), m, rigger.Install{Release: app.Name, Namespace: app.Namespace, Region: "eu"}, host.Settings{})
	if diags.HasErrors() || rp.Action != rigger.ActionInstall || rp.TakesOver != "" {
		t.Fatalf("PlanInstall() = %+v, %v; want an install that takes nothing over", rp, diags)
	}

	// A stack installs app after the plan.
	if _, err := f.Apply(t.Context(), host.ApplyRequest{Release: app, Owner: otherStack}); err != nil {
		t.Fatal(err)
	}

	if _, err := e.ApplyInstall(t.Context(), rp, rigger.ApplyOptions{Adopt: true}); !errors.Is(err, errOwned) {
		t.Errorf("ApplyInstall() = %v, want the release of the stack refused", err)
	}

	if got := f.releases[app]; len(got) != 1 || got[0].Owner != otherStack {
		t.Errorf("app has %d revisions, the last of %q: taken over without a plan of it", len(got), got[len(got)-1].Owner)
	}
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/host"
)

var errManaged = errors.New("409: another manager applied the release")

// packCluster is a cluster where the first revision of db is another
// package manager's, which refuses a request that does not adopt it.
type packCluster struct{ *fakeCluster }

func (p packCluster) Current(ctx context.Context, ref host.ReleaseRef) (host.Revision, error) {
	rev, err := p.fakeCluster.Current(ctx, ref)
	if err == nil && ref.Name == "db" && rev.Number == 1 {
		rev.ForeignManager = "pack"
	}

	return rev, err
}

func (p packCluster) Plan(ctx context.Context, req host.ApplyRequest) (host.Plan, error) {
	if cur, err := p.Current(ctx, req.Release); err == nil && cur.ForeignManager != "" && !req.Adopt {
		return host.Plan{}, errManaged
	}

	return p.fakeCluster.Plan(ctx, req)
}

// A take-over is planned without being asked for and says so; an apply
// without Adopt stops before it writes anything.
func TestStack_TakeOver(t *testing.T) {
	t.Parallel()

	f := newFakeCluster()
	c := packCluster{f}
	db := host.ReleaseRef{Namespace: testNamespace, Name: "db"}

	if _, err := f.Apply(t.Context(), host.ApplyRequest{Release: db, Digest: "of the pack"}); err != nil {
		t.Fatal(err)
	}

	e, err := rigger.New(rigger.WithReleases(c), rigger.WithSecrets(c), rigger.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}

	plan := planOf(t, e, stackFiles(twoReleases), nil)

	if got := plan.Releases[0]; got.Release != db || !strings.Contains(got.TakesOver, "pack") {
		t.Fatalf("plan of db = %+v, want a take-over from pack", got)
	}

	_, diags := e.ApplyStack(t.Context(), plan, rigger.ApplyOptions{})
	if !strings.Contains(diags.Error(), "-adopt") {
		t.Errorf("apply without adopt = %v", diags)
	}

	if len(f.secrets) != 0 || len(f.releases) != 1 || len(f.releases[db]) != 1 {
		t.Errorf("an apply without adopt wrote: %d secrets, releases %v", len(f.secrets), f.releases)
	}

	if _, diags := e.ApplyStack(t.Context(), plan, rigger.ApplyOptions{Adopt: true}); diags.HasErrors() {
		t.Fatal(diags)
	}

	if got := actions(planOf(t, e, stackFiles(twoReleases), nil)); got != bothUnchanged {
		t.Errorf("the plan after the take-over = %s", got)
	}
}

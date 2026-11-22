// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"context"
	"log/slog"
	"testing"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/host"
)

// phaseCluster is a cluster whose releases are in one phase.
type phaseCluster struct {
	*fakeCluster

	phase string
}

func (p phaseCluster) Current(ctx context.Context, ref host.ReleaseRef) (host.Revision, error) {
	rev, err := p.fakeCluster.Current(ctx, ref)
	if err == nil {
		rev.Status = p.phase
	}

	return rev, err
}

// Only a release that was deployed is unchanged: one that failed, or was
// left on the way, is applied again by the same content.
func TestPlanStack_RetriesWhatDidNotSucceed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		phase    string
		expected rigger.ReleaseAction
	}{
		{phase: "deployed", expected: rigger.ActionUnchanged},
		{phase: "failed", expected: rigger.ActionUpgrade},
		{phase: "pending", expected: rigger.ActionUpgrade},
		{phase: "applying", expected: rigger.ActionUpgrade},
		{phase: "rolling-back", expected: rigger.ActionUpgrade},
		{phase: "uninstalling", expected: rigger.ActionUpgrade},
		{phase: "", expected: rigger.ActionUpgrade},
	}

	for _, tt := range tests {
		t.Run("phase "+tt.phase, func(t *testing.T) {
			t.Parallel()

			f := newFakeCluster()

			if _, diags := stackEngine(t, f).ApplyStack(t.Context(), planOf(t, stackEngine(t, f), stackFiles(twoReleases), nil), rigger.ApplyOptions{}); diags.HasErrors() {
				t.Fatal(diags)
			}

			c := phaseCluster{fakeCluster: f, phase: tt.phase}

			e, err := rigger.New(rigger.WithReleases(c), rigger.WithSecrets(c), rigger.WithLogger(slog.New(slog.DiscardHandler)))
			if err != nil {
				t.Fatal(err)
			}

			for _, rp := range planOf(t, e, stackFiles(twoReleases), nil).Releases {
				if rp.Action != tt.expected {
					t.Errorf("%s = %s, want %s", rp.Release.Name, rp.Action, tt.expected)
				}
			}
		})
	}
}

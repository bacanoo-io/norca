// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"context"
	"log/slog"
	"maps"
	"strings"
	"testing"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/host"
)

// otherRunSecrets is a secret store where another run writes the generated
// values between the read and the write of this one.
type otherRunSecrets struct{ *fakeCluster }

func (o otherRunSecrets) Ensure(ctx context.Context, s host.Secret) (bool, error) {
	if strings.HasSuffix(s.Path, "/generated") {
		o.mu.Lock()

		items := maps.Clone(o.secrets[s.Path])
		if items == nil {
			items = map[string]host.Sensitive{}
		}

		items["db"] = host.NewSensitive("of the other run")
		o.secrets[s.Path] = items
		o.mu.Unlock()
	}

	return o.fakeCluster.Ensure(ctx, s)
}

// Of two runs that generate, the one that loses applies nothing: the
// password it made is not the stored one. Both for the first generated
// value of a stack and for one added to those it has.
func TestApplyStack_GeneratedMeanwhile(t *testing.T) {
	t.Parallel()

	const stored = "rigger/stacks/shop/generated"

	tests := []struct {
		name string
		had  map[string]host.Sensitive
	}{
		{name: "first"},
		{name: "added", had: map[string]host.Sensitive{"older": host.NewSensitive("kept")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFakeCluster()
			if tt.had != nil {
				f.secrets[stored] = maps.Clone(tt.had)
			}

			c := otherRunSecrets{f}

			e, err := rigger.New(rigger.WithReleases(c), rigger.WithSecrets(c), rigger.WithLogger(slog.New(slog.DiscardHandler)))
			if err != nil {
				t.Fatal(err)
			}

			_, diags := e.ApplyStack(t.Context(), planOf(t, e, stackFiles(twoReleases), nil), rigger.ApplyOptions{})
			if !strings.Contains(diags.Error(), rigger.ErrGeneratedMeanwhile.Error()) {
				t.Errorf("apply = %v, want it stopped", diags)
			}

			if len(f.releases) != 0 {
				t.Errorf("releases were applied with values that are not stored: %v", f.releases)
			}

			if got := f.secrets[stored]["db"]; got.Reveal() != "of the other run" {
				t.Errorf("the value of the other run was overwritten by %q", got.Reveal())
			}
		})
	}
}

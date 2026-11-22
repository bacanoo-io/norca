// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	rigger "github.com/bacanoo-io/norca/rigger"
)

// depends_on names what the stack has: a name it does not have is an error
// on a release and on a part, for a release and for a part.
func TestStack_DependsOnUnknown(t *testing.T) {
	t.Parallel()

	const (
		release        = `source = "./modules/app"`
		part           = `source  = "acme/data"`
		unknownPart    = "stack.typo"
		unknownRelease = "module.typo"
	)

	tests := []struct {
		name, after, depends string
	}{
		{name: "release on a part", after: release, depends: unknownPart},
		{name: "release on a release", after: release, depends: unknownRelease},
		{name: "part on a part", after: part, depends: unknownPart},
		{name: "part on a release", after: part, depends: unknownRelease},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFakeCluster()

			e, err := rigger.New(rigger.WithReleases(f), rigger.WithSecrets(f), rigger.WithLogger(slog.New(slog.DiscardHandler)),
				rigger.WithFetcher(repoFetcher{tree: publishedData(), version: publishedVersion}))
			if err != nil {
				t.Fatal(err)
			}

			files := stackFiles(strings.Replace(siteStack, tt.after, tt.after+"\n  depends_on = ["+tt.depends+"]", 1))

			_, lock, diags := e.SyncStack(t.Context(), files, "s", false)
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			files["s/rigger.lock.hcl"] = &fstest.MapFile{Data: lock}

			s, diags := e.LoadStack(t.Context(), files, "s")
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			_, diags = e.PlanStack(t.Context(), s, nil)
			if !strings.Contains(diags.Error(), "depends_on names "+tt.depends) {
				t.Errorf("plan = %v, want the unknown %s refused", diags, tt.depends)
			}
		})
	}
}

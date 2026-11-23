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

// acmeRegistry is the head of a stack with the registry acme.
const acmeRegistry = "rigger {\n  name = \"site\"\n\n  registry \"acme\" {\n    source = \"github.com/acme/rigger\"\n  }\n}\n"

// lockPath is the lock file of the stack of the tests.
const lockPath = "s/rigger.lock.hcl"

// webTree is a repository with the module web 1.0.0.
func webTree() fstest.MapFS {
	return fstest.MapFS{"modules/web/" + metadataFile: {Data: []byte("module {\n  name    = \"web\"\n  version = \"1.0.0\"\n}\n")}}
}

// newSyncEngine is an engine over a cluster in memory that fetches the one
// version of tree.
func newSyncEngine(t *testing.T, tree fstest.MapFS) *rigger.Engine {
	t.Helper()

	f := newFakeCluster()

	e, err := rigger.New(rigger.WithReleases(f), rigger.WithSecrets(f), rigger.WithLogger(slog.New(slog.DiscardHandler)),
		rigger.WithFetcher(repoFetcher{tree: tree, version: publishedVersion}))
	if err != nil {
		t.Fatal(err)
	}

	return e
}

// A sync returns the stack a load with the new lock file returns: a source
// that more than one release or part names is loaded for each of them.
func TestSyncStack_SharedSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		stack    string
		tree     fstest.MapFS
		expected string
	}{
		{
			name: "two releases of one module",
			stack: acmeRegistry +
				"\nmodule \"one\" {\n  source  = \"acme/web\"\n  version = \"~> 1.0\"\n}\n" +
				"\nmodule \"two\" {\n  source  = \"acme/web\"\n  version = \"~> 1.0\"\n}\n",
			tree:     webTree(),
			expected: "one:install two:install",
		},
		{
			name: "two parts of one stack",
			stack: acmeRegistry +
				"\nstack \"data\" {\n  source  = \"acme/data\"\n  version = \"~> 1.0\"\n}\n" +
				"\nstack \"more\" {\n  source  = \"acme/data\"\n  version = \"~> 1.0\"\n}\n",
			tree:     publishedData(),
			expected: "data-db:install more-db:install",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := newSyncEngine(t, tt.tree)
			files := stackFiles(tt.stack)

			synced, lock, diags := e.SyncStack(t.Context(), files, "s", false)
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			if diags := e.ValidateStack(t.Context(), synced, nil); diags.HasErrors() {
				t.Errorf("the stack of the sync does not validate: %v", diags)
			}

			plan, diags := e.PlanStack(t.Context(), synced, nil)
			if diags.HasErrors() {
				t.Fatalf("the stack of the sync is not planned: %v", diags)
			}

			if got := actions(plan); got != tt.expected {
				t.Errorf("plan of the stack of the sync = %s, want %s", got, tt.expected)
			}

			files[lockPath] = &fstest.MapFile{Data: lock}

			if got := actions(planOf(t, e, files, nil)); got != tt.expected {
				t.Errorf("plan of the stack loaded with the lock = %s, want %s", got, tt.expected)
			}
		})
	}
}

// A source has one version in a stack: a second release or part whose
// constraint the version of the first does not meet is an error.
func TestSyncStack_ConstraintsDiffer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		stack    string
		tree     fstest.MapFS
		expected string
	}{
		{
			name: "releases",
			stack: acmeRegistry +
				"\nmodule \"one\" {\n  source  = \"acme/web\"\n  version = \"~> 1.0\"\n}\n" +
				"\nmodule \"two\" {\n  source  = \"acme/web\"\n  version = \"~> 2.0\"\n}\n",
			tree:     webTree(),
			expected: "acme/web is used by more than one release",
		},
		{
			name: "parts",
			stack: acmeRegistry +
				"\nstack \"data\" {\n  source  = \"acme/data\"\n  version = \"~> 1.0\"\n}\n" +
				"\nstack \"more\" {\n  source  = \"acme/data\"\n  version = \"~> 2.0\"\n}\n",
			tree:     publishedData(),
			expected: "acme/data is a part more than once",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s, _, diags := newSyncEngine(t, tt.tree).SyncStack(t.Context(), stackFiles(tt.stack), "s", false)
			if s != nil || !strings.Contains(diags.Error(), tt.expected) {
				t.Errorf("SyncStack() = %v, %q, want no stack and %q", s, diags.Error(), tt.expected)
			}
		})
	}
}

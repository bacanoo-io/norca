// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/bundle"
	"github.com/bacanoo-io/norca/rigger/fetch"
	"github.com/bacanoo-io/norca/rigger/source"
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
func newSyncEngine(t *testing.T, tree fstest.MapFS, opts ...rigger.Option) *rigger.Engine {
	t.Helper()

	f := newFakeCluster()

	e, err := rigger.New(append([]rigger.Option{
		rigger.WithReleases(f), rigger.WithSecrets(f), rigger.WithLogger(slog.New(slog.DiscardHandler)),
		rigger.WithFetcher(repoFetcher{tree: tree, version: publishedVersion}),
	}, opts...)...)
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

// The repositories of the registry of TestSyncStack_RegistryMoved. Neither
// can be reached, so a fetcher with a cache reads only what its cache has.
const (
	firstRegistry  = "https://127.0.0.1:1/acme/rigger"
	mirrorRegistry = "https://127.0.0.1:1/mirror/rigger"
)

// importBundle bundles the modules and the parts of a stack from sources
// and puts them into the cache of a fetcher where the import of a bundle
// puts them: by the repository, the commit, the directory and the digest
// that the lock file records.
func importBundle(t *testing.T, f *fetch.Fetcher, s *rigger.Stack) {
	t.Helper()

	var buf bytes.Buffer

	if err := bundle.Write(&buf, s.Name(), s.SourcedModules()); err != nil {
		t.Fatal(err)
	}

	manifest, files, err := bundle.Read(&buf)
	if err != nil {
		t.Fatal(err)
	}

	for _, m := range manifest.Modules {
		if err := bundle.WriteTree(f.BundleDir(m.Repository, m.Commit, m.Path, m.Digest), bundle.ModuleFiles(m, files)); err != nil {
			t.Fatal(err)
		}
	}
}

// A sync that keeps the locked version records where it fetched the version
// now: when the registry moved (to a mirror), the lock file names the new
// repository. A bundle of the stack is kept by the repository of the lock
// and looked for by that of the registry, so it is found without a network
// only when the two are one.
func TestSyncStack_RegistryMoved(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		tree fstest.MapFS
	}{
		{name: "a module", body: "\nmodule \"one\" {\n  source  = \"acme/web\"\n  version = \"~> 1.0\"\n}\n", tree: webTree()},
		{name: "a part", body: "\nstack \"data\" {\n  source  = \"acme/data\"\n  version = \"~> 1.0\"\n}\n", tree: publishedData()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stackAt := func(repository string) *fstest.MapFile {
				return &fstest.MapFile{Data: []byte(strings.Replace(acmeRegistry, "github.com/acme/rigger", repository, 1) + tt.body)}
			}

			e := newSyncEngine(t, tt.tree)
			files := stackFiles("")
			files[stackPath] = stackAt(firstRegistry)

			_, lock, diags := e.SyncStack(t.Context(), files, "s", false)
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			if !strings.Contains(string(lock), firstRegistry) {
				t.Fatalf("the lock does not name the repository of the registry:\n%s", lock)
			}

			files[lockPath] = &fstest.MapFile{Data: lock}
			files[stackPath] = stackAt(mirrorRegistry)

			moved, lock, diags := e.SyncStack(t.Context(), files, "s", false)
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			if strings.Contains(string(lock), firstRegistry) || !strings.Contains(string(lock), mirrorRegistry) {
				t.Errorf("the lock after the registry moved to %s:\n%s", mirrorRegistry, lock)
			}

			files[lockPath] = &fstest.MapFile{Data: lock}
			cache := fetch.New(t.TempDir())
			importBundle(t, cache, moved)

			offline, err := rigger.New(rigger.WithLogger(slog.New(slog.DiscardHandler)), rigger.WithFetcher(cache))
			if err != nil {
				t.Fatal(err)
			}

			if _, diags := offline.LoadStack(t.Context(), files, "s"); diags.HasErrors() {
				t.Errorf("a load from the bundle without a network: %v", diags)
			}
		})
	}
}

// globalFetcher serves the module web as the global registry does: with its
// index it says the repository of the version; without it (repository is
// empty) the request names the repository, the one the lock file records.
type globalFetcher struct{ repository string }

func (globalFetcher) Versions(context.Context, source.Request) ([]source.Available, error) {
	return []source.Available{{Version: publishedVersion, Ref: "v" + publishedVersion, Commit: partCommit}}, nil
}

func (f globalFetcher) Fetch(_ context.Context, req source.Request) (source.Fetched, error) {
	return source.Fetched{
		FS: webTree(), Dir: "modules/web",
		Origin: source.Origin{Source: req.Source, Version: req.Version, Ref: req.Ref, Commit: partCommit, Repository: f.repository},
	}, nil
}

// A module of the global registry has no repository in its address: a sync
// that keeps its version keeps the repository of the lock file when the
// index can't be read, and records the one the index lists when it can.
func TestSyncStack_GlobalRepository(t *testing.T) {
	t.Parallel()

	const listed = "github.com/author/modules"

	tests := []struct {
		name     string
		second   string
		expected string
	}{
		{name: "without the index", second: "", expected: listed},
		{name: "the index lists the same", second: listed, expected: listed},
		{name: "the index lists another", second: "github.com/author/moved", expected: "github.com/author/moved"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sync := func(files fstest.MapFS, repository string) []byte {
				e, err := rigger.New(rigger.WithLogger(slog.New(slog.DiscardHandler)), rigger.WithFetcher(globalFetcher{repository: repository}))
				if err != nil {
					t.Fatal(err)
				}

				_, lock, diags := e.SyncStack(t.Context(), files, "s", false)
				if diags.HasErrors() {
					t.Fatal(diags)
				}

				return lock
			}

			files := stackFiles("rigger {\n  name = \"site\"\n}\n\nmodule \"web\" {\n  source = \"author/web\"\n}\n")

			first := sync(files, listed)
			if !strings.Contains(string(first), `repository = "`+listed+`"`) {
				t.Fatalf("the lock does not name the repository the index lists:\n%s", first)
			}

			files[lockPath] = &fstest.MapFile{Data: first}

			if second := sync(files, tt.second); !strings.Contains(string(second), `repository = "`+tt.expected+`"`) {
				t.Errorf("the lock of the second sync does not name %s:\n%s", tt.expected, second)
			}
		})
	}
}

// wideStack is a repository with the published stack wide 1.0.0: one
// release, of the module top of moduleTree.
func wideStack(width, levels int) fstest.MapFS {
	tree := fstest.MapFS{
		"stacks/wide/" + metadataFile: {Data: []byte("stack {\n  name    = \"wide\"\n  version = \"1.0.0\"\n}\n")},
		"stacks/wide/stack.hcl":       {Data: []byte("rigger {\n  name = \"wide\"\n}\n\nmodule \"top\" {\n  source = \"./modules/top\"\n}\n")},
	}

	for name, f := range moduleTree(width, levels) {
		tree["stacks/wide/"+name] = f
	}

	return tree
}

// stackOf is a stack with the registry acme and a block of the type for each
// source, named b0, b1 and so on.
func stackOf(blockType string, sources ...string) string {
	var b strings.Builder

	b.WriteString(acmeRegistry)

	for i, src := range sources {
		fmt.Fprintf(&b, "\n%s \"b%d\" {\n  source = %q\n}\n", blockType, i, src)
	}

	return b.String()
}

// A sync bounds the modules of a stack as a load does (Limits.Modules): the
// modules of its sources and of its parts count with its own, each as often
// as a release or a part uses it. So the lock file a sync writes loads on
// the same engine.
func TestSyncStack_ModulesBound(t *testing.T) {
	t.Parallel()

	// The module top of moduleTree(4, 4) is 341 modules; three of them are
	// more than the 1000 of the engine.
	const (
		top     = "github.com/acme/rigger//modules/top"
		release = "module"
	)

	tests := []struct {
		name  string
		stack string
		tree  fstest.MapFS
		// local puts the module top into the stack too.
		local bool
	}{
		{name: "releases of one source", stack: stackOf(release, top, top, top), tree: moduleTree(4, 4)},
		{
			name:  "releases of three sources",
			stack: stackOf(release, top, "github.com/acme/second//modules/top", "github.com/acme/third//modules/top"),
			tree:  moduleTree(4, 4),
		},
		{name: "local modules and a source", stack: stackOf(release, "./modules/top", "./modules/top", top), tree: moduleTree(4, 4), local: true},
		{name: "parts of one stack", stack: stackOf("stack", "acme/wide", "acme/wide", "acme/wide"), tree: wideStack(4, 4)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			files := stackFiles(tt.stack)

			if tt.local {
				for name, f := range moduleTree(4, 4) {
					files["s/"+name] = f
				}
			}

			if _, _, diags := newSyncEngine(t, tt.tree).SyncStack(t.Context(), files, "s", false); !strings.Contains(diags.Error(), tooManyModules) {
				t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), tooManyModules)
			}

			// The stack is in order: an engine of a wider bound syncs it.
			limits := rigger.DefaultLimits()
			limits.Modules = 2000

			if _, _, diags := newSyncEngine(t, tt.tree, rigger.WithLimits(limits)).SyncStack(t.Context(), files, "s", false); diags.HasErrors() {
				t.Errorf("a sync with a bound of %d modules: %v", limits.Modules, diags)
			}
		})
	}
}

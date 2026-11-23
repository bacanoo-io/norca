// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"testing"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/cli/norcaapi"
	"github.com/bacanoo-io/norca/rigger/source"
)

// examplesDir is examples/ of Rigger, seen from this package.
const examplesDir = "../../examples"

// TestEngine_ValidateModule_Examples renders the value files of the
// examples with the job decoder of Norca (design 7.4).
func TestEngine_ValidateModule_Examples(t *testing.T) {
	t.Parallel()

	for _, dir := range []string{
		"module/web", "registry/modules/redis", "registry/modules/nats", "registry/modules/postgres",
	} {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()

			eng, err := rigger.New(rigger.WithJobDecoder(norcaapi.JobDecoder{}))
			if err != nil {
				t.Fatal(err)
			}

			m, diags := eng.LoadModuleAt(t.Context(), os.DirFS("../../examples"), dir)
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			if diags := eng.ValidateModule(t.Context(), m); diags.HasErrors() {
				t.Error(diags)
			}
		})
	}
}

// TestEngine_TestModule_Examples runs the tests of the examples with the
// job decoder of Norca: every test passes.
func TestEngine_TestModule_Examples(t *testing.T) {
	t.Parallel()

	for _, dir := range []string{
		"module/web", "registry/modules/redis", "registry/modules/nats", "registry/modules/postgres",
	} {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()

			eng, err := rigger.New(rigger.WithJobDecoder(norcaapi.JobDecoder{}))
			if err != nil {
				t.Fatal(err)
			}

			m, diags := eng.LoadModuleAt(t.Context(), os.DirFS("../../examples"), dir)
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			results, diags := eng.TestModule(t.Context(), m)
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			if len(results) == 0 {
				t.Fatal("no tests")
			}

			for _, r := range results {
				if !r.Passed() {
					t.Errorf("%s: %s: %v %v", r.File, r.Name, r.Failures, r.Diagnostics)
				}
			}
		})
	}
}

// exampleRepository is the repository of the registry "example" of the
// stacks of examples/. No such repository is on the network:
// examples/registry is what it holds.
const exampleRepository = "github.com/example/rigger-registry"

// exampleRegistry serves examples/registry to an engine in the place of
// the repository of the example registry: the files as they are, for the
// version a lock file names. The engine takes them only with the tree
// digest the lock file records and signed as the trust of the stack asks,
// so a lock file or a signature that a module has outgrown fails the load.
// (That the lock file is the one a sync writes, commit and tag included, is
// TestSyncStack_Examples of the library, over a git repository.) Nothing
// else is fetched: the tests of the examples use no network.
type exampleRegistry struct{}

func (exampleRegistry) Versions(_ context.Context, req source.Request) ([]source.Available, error) {
	return nil, fmt.Errorf("%s: the examples are loaded by their lock files, no version is resolved: %w", req.Source, fs.ErrNotExist)
}

func (exampleRegistry) Fetch(_ context.Context, req source.Request) (source.Fetched, error) {
	if req.Kind != source.KindGit || req.Repository != exampleRepository {
		return source.Fetched{}, fmt.Errorf("%s (%s %q) is not of %s, the one registry examples/ holds; the tests fetch nothing over a network: %w",
			req.Source, req.Kind, req.Repository, exampleRepository, fs.ErrNotExist)
	}

	return source.Fetched{
		FS: os.DirFS(examplesDir + "/registry"), Dir: req.Path,
		Origin: source.Origin{Source: req.Source, Version: req.Version, Ref: req.Ref, Commit: req.Commit},
	}, nil
}

// notLoaded are the stacks of examples/ that are not loaded with their
// modules, each with the reason.
var notLoaded = map[string]string{
	"registry/stacks/observability": "its modules are of another registry (github.com/bacanoo-io/norca-registry), " +
		"which examples/ does not hold",
}

// exampleStacks returns the directories of the stacks of examples/, found
// by walking: a stack that is added is tested without a change of the tests.
func exampleStacks(t *testing.T) []string {
	t.Helper()

	var dirs []string

	err := fs.WalkDir(os.DirFS(examplesDir), ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "stack.hcl" {
			dirs = append(dirs, path.Dir(p))
		}

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(dirs) == 0 {
		t.Fatal("examples/ has no stack")
	}

	return dirs
}

// TestEngine_ValidateStack_Examples is rigger stack validate of the stacks
// of examples/, with the decoders of Norca: a stack loads with its lock
// file, the modules of the example registry among them, and evaluates with
// each of its value files (values/ of a stack of one's own, examples/ of a
// published stack), without a diagnostic.
func TestEngine_ValidateStack_Examples(t *testing.T) {
	t.Parallel()

	for _, dir := range exampleStacks(t) {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()

			if reason, ok := notLoaded[dir]; ok {
				t.Skip("not loaded: " + reason)
			}

			eng, err := rigger.New(rigger.WithJobDecoder(norcaapi.JobDecoder{}), rigger.WithResourceDecoder(norcaapi.ResourceDecoder{}),
				rigger.WithFetcher(exampleRegistry{}))
			if err != nil {
				t.Fatal(err)
			}

			s, diags := eng.LoadStack(t.Context(), os.DirFS(examplesDir), dir)
			for _, d := range diags {
				t.Errorf("LoadStack(): %s", d)
			}

			if diags.HasErrors() {
				return
			}

			examples, err := fs.Glob(os.DirFS(path.Join(examplesDir, dir)), "examples/*.vars.hcl")
			if err != nil {
				t.Fatal(err)
			}

			files := slices.Concat(s.ValueFiles(), examples)
			if len(files) == 0 {
				for _, d := range eng.ValidateStack(t.Context(), s, nil) {
					t.Errorf("without values: %s", d)
				}
			}

			for _, p := range files {
				src, err := s.ReadValueFile(p)
				if err != nil {
					t.Fatal(err)
				}

				values, diags := rigger.ParseValues(p, src)
				if !diags.HasErrors() {
					diags = append(diags, eng.ValidateStack(t.Context(), s, values)...)
				}

				for _, d := range diags {
					t.Errorf("%s: %s", p, d)
				}
			}
		})
	}
}

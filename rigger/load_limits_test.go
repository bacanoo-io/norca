// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	rigger "github.com/bacanoo-io/norca/rigger"
)

// tooManyModules is the message of the bound of the modules of a load.
const tooManyModules = "more than 1000 modules"

// moduleTree is the module top that calls the next level width times, and
// so on: a few files of which each level multiplies the modules.
func moduleTree(width, levels int) fstest.MapFS {
	files := fstest.MapFS{}

	for level := range levels + 1 {
		name, dir := fmt.Sprintf("l%d", level), fmt.Sprintf("modules/l%d/", level)
		if level == 0 {
			name, dir = "top", "modules/top/"
		}

		files[dir+metadataFile] = &fstest.MapFile{Data: []byte("module {\n  name    = \"" + name + "\"\n  version = \"1.0.0\"\n}\n")}

		if level == levels {
			break
		}

		var calls strings.Builder
		for i := range width {
			fmt.Fprintf(&calls, "module \"c%d\" {\n  source = \"../l%d\"\n}\n", i, level+1)
		}

		files[dir+"modules.hcl"] = &fstest.MapFile{Data: []byte(calls.String())}
	}

	return files
}

// The depth of child modules does not bound their number.
func TestEngine_LoadModuleWide(t *testing.T) {
	t.Parallel()

	// 1365 modules of six files.
	if _, diags := engine(t).LoadModuleAt(t.Context(), moduleTree(4, 5), "modules/top"); !strings.Contains(diags.Error(), tooManyModules) {
		t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), tooManyModules)
	}
}

// What is parsed is bounded for a whole tree, not for each module alone.
func TestEngine_LoadModuleBytes(t *testing.T) {
	t.Parallel()

	limits := rigger.DefaultLimits()
	limits.ModuleBytes = 64 << 10

	e, err := rigger.New(rigger.WithLimits(limits), rigger.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}

	files := moduleTree(10, 1)
	files["modules/l1/locals.hcl"] = &fstest.MapFile{Data: []byte("# " + strings.Repeat("x", 40<<10))}

	if _, diags := e.LoadModuleAt(t.Context(), files, "modules/top"); !strings.Contains(diags.Error(), "Modules too large") {
		t.Errorf("diagnostics = %q, want the files of the tree bounded", diags.Error())
	}
}

// The releases of a stack share the bound.
func TestEngine_LoadStackWide(t *testing.T) {
	t.Parallel()

	var stack strings.Builder

	stack.WriteString("rigger {\n  name = \"wide\"\n}\n")

	for i := range 1001 {
		fmt.Fprintf(&stack, "module \"r%d\" {\n  source = \"./modules/app\"\n}\n", i)
	}

	if _, diags := engine(t).LoadStack(t.Context(), stackFiles(stack.String()), "s"); !strings.Contains(diags.Error(), tooManyModules) {
		t.Errorf("diagnostics = %.200q, want them to contain %q", diags.Error(), tooManyModules)
	}
}

// So do the modules a lock file pins, which are fetched one by one.
func TestEngine_LoadStackLockedWide(t *testing.T) {
	t.Parallel()

	e, err := rigger.New(rigger.WithLogger(slog.New(slog.DiscardHandler)),
		rigger.WithFetcher(repoFetcher{tree: moduleTree(4, 4), version: publishedVersion}))
	if err != nil {
		t.Fatal(err)
	}

	var stack strings.Builder

	stack.WriteString("rigger {\n  name = \"wide\"\n}\n")

	// 341 modules each.
	for i := range 3 {
		fmt.Fprintf(&stack, "module \"r%d\" {\n  source = \"github.com/acme/rigger//modules/top\"\n}\n", i)
	}

	files := stackFiles(stack.String())

	_, lock, diags := e.SyncStack(t.Context(), files, "s", false)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	files["s/rigger.lock.hcl"] = &fstest.MapFile{Data: lock}

	if _, diags := e.LoadStack(t.Context(), files, "s"); !strings.Contains(diags.Error(), tooManyModules) {
		t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), tooManyModules)
	}
}

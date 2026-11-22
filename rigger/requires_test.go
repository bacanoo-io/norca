// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/host"
)

// norcaCluster is a cluster of a version of Norca.
type norcaCluster struct{}

func (norcaCluster) Info(context.Context) (host.ClusterInfo, error) {
	return host.ClusterInfo{Version: norcaVersion, Region: "eu"}, nil
}

func (norcaCluster) Namespaces(context.Context) ([]string, error) {
	return []string{testNamespace}, nil
}

const (
	// norcaVersion is the version of the cluster of these tests.
	norcaVersion = "0.3.0"
	storeModule  = "rustfs"
	needsStack   = "rigger {\n  name      = \"shop\"\n  region    = \"eu\"\n  namespace = \"apps\"\n}\n\nmodule \"needs\" {\n  source = \"./modules/needs\"\n  values = { dumps = %DUMPS% }\n}\n"
	storeBlock   = "\nmodule \"store\" {\n  source = \"./modules/store\"\n}\n"
)

func requiringModule(when string) *fstest.MapFile {
	if when != "" {
		when = "\n      when    = \"" + when + "\""
	}

	return &fstest.MapFile{Data: []byte("module {\n  name    = \"needs\"\n  version = \"1.0.0\"\n\n  requires {\n    module \"" + storeModule +
		"\" {\n      version = \">= 0.2.0\"" + when + "\n    }\n  }\n}\n")}
}

func requiresEngine(t *testing.T, f *fakeCluster, opts ...rigger.Option) *rigger.Engine {
	t.Helper()

	e, err := rigger.New(append([]rigger.Option{
		rigger.WithReleases(f), rigger.WithSecrets(f), rigger.WithLogger(slog.New(slog.DiscardHandler)),
	}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}

	return e
}

// A module that requires another is planned only next to one that fits: in
// the stack, or installed in its namespace.
func TestPlanStack_RequiresModule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		inStack   string // the version of the store in the stack
		installed string // of the one installed in the namespace
		when      string
		dumps     bool
		expected  string // in the error; empty for none
	}{
		{name: "missing", expected: storeModule},
		{name: "in the stack", inStack: "0.2.1"},
		{name: "too old in the stack", inStack: "0.1.0", expected: storeModule},
		{name: "installed", installed: norcaVersion},
		{name: "too old installed", installed: "0.1.0", expected: storeModule},
		{name: "not asked for", when: "dumps"},
		{name: "asked for", when: "dumps", dumps: true, expected: storeModule},
		{name: "asked by var.", when: "var.dumps", dumps: true, expected: storeModule},
		{name: "no such variable", when: "dmups", expected: "dmups"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stack, dumps := needsStack, "false"
			if tt.dumps {
				dumps = "true"
			}

			if tt.inStack != "" {
				stack += storeBlock
			}

			files := stackFiles(strings.Replace(stack, "%DUMPS%", dumps, 1))
			files["s/modules/needs/"+metadataFile] = requiringModule(tt.when)
			files["s/modules/needs/"+variablesFile] = &fstest.MapFile{Data: []byte("variable \"dumps\" {\n  default = false\n}\n")}

			if tt.inStack != "" {
				files["s/modules/store/"+metadataFile] = &fstest.MapFile{
					Data: []byte("module {\n  name    = \"" + storeModule + "\"\n  version = \"" + tt.inStack + "\"\n}\n"),
				}
			}

			f := newFakeCluster()

			if tt.installed != "" {
				if _, err := f.Apply(t.Context(), host.ApplyRequest{
					Release: host.ReleaseRef{Namespace: testNamespace, Name: "s3"},
					Module:  host.ModuleRef{Name: storeModule, Version: tt.installed},
				}); err != nil {
					t.Fatal(err)
				}
			}

			e := requiresEngine(t, f)

			s, diags := e.LoadStack(t.Context(), files, "s")
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			_, diags = e.PlanStack(t.Context(), s, nil)

			switch {
			case tt.expected == "" && diags.HasErrors():
				t.Errorf("plan = %v, want none", diags)
			case tt.expected != "" && !strings.Contains(diags.Error(), tt.expected):
				t.Errorf("plan = %v, want %q", diags, tt.expected)
			}
		})
	}
}

// An install alone looks at the installed releases of its namespace.
func TestPlanInstall_RequiresModule(t *testing.T) {
	t.Parallel()

	f := newFakeCluster()
	e := requiresEngine(t, f)

	m, diags := e.LoadModule(t.Context(), fstest.MapFS{metadataFile: requiringModule("")})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	in := rigger.Install{Release: "needs", Namespace: testNamespace, Region: "eu"}

	if _, diags := e.PlanInstall(t.Context(), m, in, host.Settings{}); !strings.Contains(diags.Error(), storeModule) {
		t.Errorf("install without it = %v", diags)
	}

	if _, err := f.Apply(t.Context(), host.ApplyRequest{
		Release: host.ReleaseRef{Namespace: testNamespace, Name: "s3"},
		Module:  host.ModuleRef{Name: storeModule, Version: "0.2.0"},
	}); err != nil {
		t.Fatal(err)
	}

	if _, diags := e.PlanInstall(t.Context(), m, in, host.Settings{}); diags.HasErrors() {
		t.Errorf("install next to it = %v", diags)
	}
}

// required_version is of Norca: of the cluster a stack is planned for.
func TestPlanStack_RequiredVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, constraint string
		noCluster        bool
		expected         string
	}{
		{name: "met", constraint: ">= 0.1.0"},
		{name: "not met", constraint: ">= 999.0.0", expected: norcaVersion},
		{name: "invalid", constraint: "newest", expected: "newest"},
		{name: "no cluster to ask", constraint: ">= 0.1.0", noCluster: true, expected: "required_version"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var opts []rigger.Option
			if !tt.noCluster {
				opts = append(opts, rigger.WithCluster(norcaCluster{}))
			}

			e := requiresEngine(t, newFakeCluster(), opts...)
			files := stackFiles(strings.Replace(twoReleases, "rigger {\n", "rigger {\n  required_version = \""+tt.constraint+"\"\n", 1))

			s, diags := e.LoadStack(t.Context(), files, "s")
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			_, diags = e.PlanStack(t.Context(), s, nil)

			switch {
			case tt.expected == "" && diags.HasErrors():
				t.Errorf("plan = %v, want none", diags)
			case tt.expected != "" && !strings.Contains(diags.Error(), tt.expected):
				t.Errorf("plan = %v, want %q", diags, tt.expected)
			}
		})
	}
}

// A part says the version it requires itself.
func TestPlanStack_RequiredVersionOfAPart(t *testing.T) {
	t.Parallel()

	tree := publishedData()
	tree["stacks/data/stack.hcl"] = &fstest.MapFile{Data: []byte(strings.Replace(string(tree["stacks/data/stack.hcl"].Data),
		"rigger {\n", "rigger {\n  required_version = \">= 999.0.0\"\n", 1))}

	e := requiresEngine(t, newFakeCluster(), rigger.WithCluster(norcaCluster{}),
		rigger.WithFetcher(repoFetcher{tree: tree, version: publishedVersion}))

	files := stackFiles(siteStack)

	_, lock, diags := e.SyncStack(t.Context(), files, "s", false)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	files["s/rigger.lock.hcl"] = &fstest.MapFile{Data: lock}

	s, diags := e.LoadStack(t.Context(), files, "s")
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if _, diags := e.PlanStack(t.Context(), s, nil); !strings.Contains(diags.Error(), "999.0.0") {
		t.Errorf("plan = %v, want the version of the part refused", diags)
	}
}

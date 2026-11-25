// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/host"
)

// fakeCluster is a release API and a secret store in memory.
type fakeCluster struct {
	mu       sync.Mutex
	releases map[host.ReleaseRef][]host.ApplyRequest
	secrets  map[string]map[string]host.Sensitive
	// removed are the releases uninstalled, in order.
	removed []string
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{releases: map[host.ReleaseRef][]host.ApplyRequest{}, secrets: map[string]map[string]host.Sensitive{}}
}

func (f *fakeCluster) revision(ref host.ReleaseRef) (host.Revision, bool) {
	revs := f.releases[ref]
	if len(revs) == 0 {
		return host.Revision{}, false
	}

	last := revs[len(revs)-1]

	return host.Revision{
		Release: ref, Number: uint64(len(revs)), Module: last.Module, Digest: last.Digest, Owner: last.Owner,
		Status: host.StatusDeployed,
	}, true
}

func (f *fakeCluster) Current(_ context.Context, ref host.ReleaseRef) (host.Revision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if rev, ok := f.revision(ref); ok {
		return rev, nil
	}

	return host.Revision{}, host.ErrNotFound
}

func (f *fakeCluster) History(ctx context.Context, ref host.ReleaseRef) ([]host.Revision, error) {
	rev, err := f.Current(ctx, ref)

	return []host.Revision{rev}, err
}

func (f *fakeCluster) Owned(_ context.Context, owner string) ([]host.Revision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []host.Revision

	for ref := range f.releases {
		if rev, ok := f.revision(ref); ok && rev.Owner == owner {
			out = append(out, rev)
		}
	}

	return out, nil
}

// List returns the current revisions of the releases of a namespace.
func (f *fakeCluster) List(_ context.Context, namespace string) ([]host.Revision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []host.Revision

	for ref := range f.releases {
		if rev, ok := f.revision(ref); ok && ref.Namespace == namespace {
			out = append(out, rev)
		}
	}

	return out, nil
}

func (f *fakeCluster) Plan(_ context.Context, req host.ApplyRequest) (host.Plan, error) {
	return host.Plan{Release: req.Release, Changes: []host.Change{{Kind: "job", Name: "x", Action: host.ActionUpdate}}}, nil
}

func (f *fakeCluster) Drift(_ context.Context, ref host.ReleaseRef) (host.Drift, error) {
	return host.Drift{Release: ref}, nil
}

func (f *fakeCluster) Apply(_ context.Context, req host.ApplyRequest) (host.Revision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.releases[req.Release] = append(f.releases[req.Release], req)
	rev, _ := f.revision(req.Release)

	return rev, nil
}

func (f *fakeCluster) Rollback(context.Context, host.ReleaseRef, uint64) (host.Revision, error) {
	return host.Revision{}, errors.New("not in the fake")
}

func (f *fakeCluster) Uninstall(_ context.Context, ref host.ReleaseRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.releases, ref)
	f.removed = append(f.removed, ref.Name)

	return nil
}

func (f *fakeCluster) Ensure(_ context.Context, s host.Secret) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if now, ok := f.secrets[s.Path]; ok && (!s.Overwrite || (s.Replaces != nil && !s.ReplacesOnly(now))) {
		return false, nil
	}

	f.secrets[s.Path] = maps.Clone(s.Items)

	return true, nil
}

func (f *fakeCluster) Read(_ context.Context, path string) (map[string]host.Sensitive, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if items, ok := f.secrets[path]; ok {
		return maps.Clone(items), nil
	}

	return nil, host.ErrNotFound
}

// stackFiles is a stack with a database and an application that reads its
// output, as local modules.
func stackFiles(stack string) fstest.MapFS {
	return fstest.MapFS{
		stackPath:                     {Data: []byte(stack)},
		"s/variables.hcl":             {Data: []byte("variable \"size\" {\n  default = 1\n}\n")},
		"s/secrets.hcl":               {Data: []byte("secret \"apps/db\" {\n  items = { password = \"!generate:db\" }\n}\n")},
		"s/modules/db/metadata.hcl":   {Data: []byte("module {\n  name    = \"db\"\n  version = \"1.0.0\"\n}\n")},
		"s/modules/db/variables.hcl":  {Data: []byte("variable \"size\" {}\n\nvariable \"password\" {\n  sensitive = true\n}\n")},
		"s/modules/db/db.var.hcl":     {Data: []byte("variable \"db\" {\n  path = \"apps/db/info\"\n\n  items {\n    size     = var.size\n    password = var.password\n  }\n}\n")},
		"s/modules/db/outputs.hcl":    {Data: []byte("output \"service\" {\n  value = \"${rigger.release}-svc\"\n}\n")},
		"s/modules/app/metadata.hcl":  {Data: []byte("module {\n  name    = \"app\"\n  version = \"1.0.0\"\n}\n")},
		"s/modules/app/variables.hcl": {Data: []byte("variable \"db\" {}\n")},
		"s/modules/app/app.var.hcl":   {Data: []byte("variable \"app\" {\n  path = \"apps/app\"\n\n  items {\n    db = var.db\n  }\n}\n")},
	}
}

// bothInstalled is the plan of twoReleases on an empty cluster.
const bothInstalled = "db:install app:install"

const twoReleases = `rigger {
  name      = "shop"
  region    = "eu"
  namespace = "apps"
}

module "app" {
  source = "./modules/app"
  values = { db = module.db.service }
}

module "db" {
  source = "./modules/db"
  values = { size = var.size, password = "!generate:db" }
}
`

// stackPath is stack.hcl of the stack of the tests, in its root.
const stackPath = "s/stack.hcl"

// metadataFile is the file of the metadata of a module or a stack.
const metadataFile = "metadata.hcl"

// variablesFile is the file of the variables of a module or a stack.
const variablesFile = "variables.hcl"

// testNamespace is the namespace of the stack of the tests.
const testNamespace = "apps"

func stackEngine(t *testing.T, f *fakeCluster) *rigger.Engine {
	t.Helper()

	e, err := rigger.New(rigger.WithReleases(f), rigger.WithSecrets(f), rigger.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}

	return e
}

func planOf(t *testing.T, e *rigger.Engine, fsys fstest.MapFS, values rigger.Values) *rigger.StackPlan {
	t.Helper()

	s, diags := e.LoadStack(t.Context(), fsys, "s")
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	plan, diags := e.PlanStack(t.Context(), s, values)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	return plan
}

func actions(plan *rigger.StackPlan) string {
	var out []string
	for _, r := range plan.Releases {
		out = append(out, r.Release.Name+":"+string(r.Action))
	}

	return strings.Join(out, " ")
}

// A directory that is none of the root holds no stack: none is returned
// with the diagnostics, not one whose every method fails.
func TestEngine_LoadStackInvalidDir(t *testing.T) {
	t.Parallel()

	e := engine(t)

	loaders := map[string]func(context.Context, fs.FS, string) (*rigger.Stack, hcl.Diagnostics){
		"LoadStack": e.LoadStack,
		"ReadStack": e.ReadStack,
	}

	for name, load := range loaders {
		for _, dir := range []string{"../s", "/s"} {
			t.Run(name+" "+dir, func(t *testing.T) {
				t.Parallel()

				s, diags := load(t.Context(), stackFiles(twoReleases), dir)
				if !strings.Contains(diags.Error(), "Stack not read") {
					t.Errorf("diagnostics = %q, want the stack not read", diags.Error())
				}

				if s != nil {
					noPanic(t, func() { t.Errorf("a stack named %q was returned, want none", s.Name()) })
				}
			})
		}
	}
}

// Every stack of examples/ reads without a diagnostic, by what a program
// reads of a stack without fetching its modules: its name, its digest, and
// of a published stack the metadata, which names it as its rigger block
// does (design 7.5).
func TestEngine_ReadStack_Examples(t *testing.T) {
	t.Parallel()

	for _, dir := range exampleStacks(t) {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()

			s, diags := engine(t).ReadStack(t.Context(), os.DirFS(examplesDir), dir)
			for _, d := range diags {
				t.Errorf("ReadStack(): %s", d)
			}

			if diags.HasErrors() {
				return
			}

			if s.Name() == "" {
				t.Error("the stack has no name")
			}

			if digest, err := s.Digest(); err != nil || !strings.HasPrefix(digest, "sha256:") {
				t.Errorf("Digest() = %q, %v", digest, err)
			}

			if md := s.Metadata(); md != nil && (md.Name != s.Name() || md.Version == "") {
				t.Errorf("the metadata names the stack %q %q, the rigger block %q", md.Name, md.Version, s.Name())
			}
		})
	}
}

func TestStack_PlanApply(t *testing.T) {
	t.Parallel()

	f := newFakeCluster()
	e := stackEngine(t, f)
	fsys := stackFiles(twoReleases)

	plan := planOf(t, e, fsys, nil)
	if got := actions(plan); got != bothInstalled {
		t.Fatalf("plan = %s, want db then app installed", got)
	}

	if strings.Join(plan.Generate, ",") != "db" || plan.Secrets[0].Action != "create" || !plan.HasChanges() {
		t.Errorf("plan generates %v, secrets %+v", plan.Generate, plan.Secrets)
	}

	if _, diags := e.ApplyStack(t.Context(), plan, rigger.ApplyOptions{}); diags.HasErrors() {
		t.Fatal(diags)
	}

	checkApplied(t, f)

	again := planOf(t, e, fsys, nil)
	if got := actions(again); got != "db:unchanged app:unchanged" || again.HasChanges() {
		t.Errorf("plan after apply = %s, generate %v, secrets %+v; want nothing to do", got, again.Generate, again.Secrets)
	}

	bigger := planOf(t, e, fsys, rigger.Values{"size": {Value: ctyNumber(3)}})
	if got := actions(bigger); got != "db:upgrade app:unchanged" || len(bigger.Releases[0].Changes) == 0 {
		t.Errorf("plan with another size = %s", got)
	}
}

// checkApplied checks what an apply of the stack wrote: the generated
// password in the secret and the secret item, not in the revision, and the
// output of db in app.
func checkApplied(t *testing.T, f *fakeCluster) {
	t.Helper()

	db := f.releases[host.ReleaseRef{Namespace: testNamespace, Name: "db"}][0]
	generated := f.secrets["rigger/stacks/shop/generated"]["db"].Reveal()

	if len(generated) != 32 || f.secrets["apps/db"]["password"].Reveal() != generated {
		t.Errorf("generated %q, secret %q: want one value of 32 characters in both", generated, f.secrets["apps/db"]["password"].Reveal())
	}

	if data, _ := json.Marshal(db.Values); strings.Contains(string(data), generated) || db.Secrets["db"]["password"].Reveal() != generated {
		t.Errorf("the revision records %s; the password must be a secret item only", data)
	}

	app := f.releases[host.ReleaseRef{Namespace: testNamespace, Name: "app"}][0]
	if !strings.Contains(string(app.Resources[0].Spec), `"db":"db-svc"`) || app.Owner != "apps/shop" {
		t.Errorf("app = %s, owner %q", app.Resources[0].Spec, app.Owner)
	}
}

func TestStack_Prune(t *testing.T) {
	t.Parallel()

	f := newFakeCluster()
	e := stackEngine(t, f)

	if _, diags := e.ApplyStack(t.Context(), planOf(t, e, stackFiles(twoReleases), nil), rigger.ApplyOptions{}); diags.HasErrors() {
		t.Fatal(diags)
	}

	onlyDB := strings.Replace(twoReleases, "module \"app\" {\n  source = \"./modules/app\"\n  values = { db = module.db.service }\n}\n", "", 1)

	plan := planOf(t, e, stackFiles(onlyDB), nil)
	if got := actions(plan); got != "db:unchanged app:remove" {
		t.Fatalf("plan = %s, want app removed", got)
	}

	out, diags := e.ApplyStack(t.Context(), plan, rigger.ApplyOptions{})
	if diags.HasErrors() || !out[1].Skipped || len(f.releases) != 2 {
		t.Fatalf("apply without prune: %+v %v; the release stays", out, diags)
	}

	if _, diags := e.ApplyStack(t.Context(), plan, rigger.ApplyOptions{Prune: true}); diags.HasErrors() || len(f.releases) != 1 {
		t.Fatalf("apply with prune left %d releases: %v", len(f.releases), diags)
	}

	s, _ := e.LoadStack(t.Context(), stackFiles(onlyDB), "s")
	if removed, err := e.DestroyStack(t.Context(), s); err != nil || len(removed) != 1 || len(f.releases) != 0 {
		t.Fatalf("destroy removed %v: %v", removed, err)
	}
}

func TestStack_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		stack    string
		expected string
	}{
		{
			name:     "a cycle",
			stack:    strings.Replace(twoReleases, "values = { size = var.size, password = \"!generate:db\" }", "values = { size = module.app.x, password = \"x\" }", 1),
			expected: "module.app → module.db → module.app",
		},
		{
			name:     "an unknown release in depends_on",
			stack:    strings.Replace(twoReleases, "source = \"./modules/app\"", "source = \"./modules/app\"\n  depends_on = [module.cache]", 1),
			expected: "depends_on names module.cache",
		},
		{
			name:     "a source of the global registry not synced",
			stack:    strings.Replace(twoReleases, "\"./modules/app\"", "\"example/app\"", 1),
			expected: "example/app has no entry in rigger.lock.hcl",
		},
		{
			name:     "an invalid release name",
			stack:    strings.Replace(twoReleases, "module \"app\"", "module \"App\"", 1),
			expected: `"App" is not a name of a release`,
		},
		{
			name:     "a value the module does not take",
			stack:    strings.Replace(twoReleases, "values = { db = module.db.service }", "values = { db = module.db.service, size = 1 }", 1),
			expected: `The module "app" has no variable "size"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := stackEngine(t, newFakeCluster())

			s, diags := e.LoadStack(t.Context(), stackFiles(tt.stack), "s")
			if !diags.HasErrors() {
				_, diags = e.PlanStack(t.Context(), s, nil)
			}

			if !strings.Contains(diags.Error(), tt.expected) {
				t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), tt.expected)
			}
		})
	}
}

func TestStack_Disabled(t *testing.T) {
	t.Parallel()

	e := stackEngine(t, newFakeCluster())
	stack := strings.Replace(twoReleases, "source = \"./modules/app\"", "source = \"./modules/app\"\n  enabled = var.size > 1", 1)

	if got := actions(planOf(t, e, stackFiles(stack), nil)); got != "db:install app:disabled" {
		t.Errorf("plan = %s, want app disabled", got)
	}
}

var _ = hcl.Diagnostics{}

func ctyNumber(n int64) cty.Value { return cty.NumberIntVal(n) }

// cancelAfterFirst is the cluster of the stack tests that cancels the
// context of the apply after it applied the first release.
type cancelAfterFirst struct {
	*fakeCluster

	cancel context.CancelFunc
}

func (c *cancelAfterFirst) Apply(ctx context.Context, req host.ApplyRequest) (host.Revision, error) {
	rev, err := c.fakeCluster.Apply(ctx, req)
	c.cancel()

	return rev, err
}

// failFirst is the cluster of the stack tests that fails every apply.
type failFirst struct {
	*fakeCluster
}

var errApplyFails = errors.New("the deployment failed")

func (c *failFirst) Apply(context.Context, host.ApplyRequest) (host.Revision, error) {
	return host.Revision{}, errApplyFails
}

// A release whose apply fails is a failed outcome, not one that is done; the
// releases after it are skipped.
func TestStack_FailedReleaseOutcome(t *testing.T) {
	t.Parallel()

	f := newFakeCluster()

	e, err := rigger.New(rigger.WithReleases(&failFirst{fakeCluster: f}), rigger.WithSecrets(f), rigger.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}

	plan := planOf(t, e, stackFiles(twoReleases), nil)

	out, diags := e.ApplyStack(t.Context(), plan, rigger.ApplyOptions{})
	if !strings.Contains(diags.Error(), errApplyFails.Error()) {
		t.Fatalf("ApplyStack() = %v, want the error of the apply", diags)
	}

	if len(out) != 2 || !out[0].Failed || out[0].Skipped || out[1].Failed || !out[1].Skipped {
		t.Fatalf("outcomes %+v, want db failed and app skipped", out)
	}
}

// A cancel stops an apply between two releases: the release being applied
// finishes, the rest are skipped; a destroy stops so too.
func TestStack_CancelBetweenReleases(t *testing.T) {
	t.Parallel()

	f := newFakeCluster()
	ctx, cancel := context.WithCancel(t.Context())
	c := &cancelAfterFirst{fakeCluster: f, cancel: cancel}

	e, err := rigger.New(rigger.WithReleases(c), rigger.WithSecrets(f), rigger.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}

	plan := planOf(t, e, stackFiles(twoReleases), nil)

	out, diags := e.ApplyStack(ctx, plan, rigger.ApplyOptions{})
	if !strings.Contains(diags.Error(), "stopped before release apps/app") || !strings.Contains(diags.Error(), context.Canceled.Error()) {
		t.Fatalf("ApplyStack() = %v, want stopped by the cancel", diags)
	}

	if len(out) != 2 || out[0].Skipped || !out[1].Skipped || len(f.releases) != 1 {
		t.Fatalf("outcomes %+v and %d releases, want db applied and app skipped", out, len(f.releases))
	}

	s, _ := e.LoadStack(t.Context(), stackFiles(twoReleases), "s")

	removed, err := e.DestroyStack(ctx, s)
	if !errors.Is(err, context.Canceled) || len(removed) != 0 || len(f.releases) != 1 {
		t.Fatalf("DestroyStack() with a cancelled context = %v, %v; want nothing removed", removed, err)
	}
}

// A destroy removes the releases in the reverse of the order of an apply:
// app reads db, so app goes first, whatever the order of the blocks; a
// release no longer in the stack goes before them.
func TestStack_DestroyOrder(t *testing.T) {
	t.Parallel()

	f := newFakeCluster()
	e := stackEngine(t, f)

	plan := planOf(t, e, stackFiles(twoReleases), nil)
	if got := actions(plan); got != bothInstalled {
		t.Fatalf("plan = %s", got)
	}

	if _, diags := e.ApplyStack(t.Context(), plan, rigger.ApplyOptions{}); diags.HasErrors() {
		t.Fatal(diags)
	}

	// A release of the stack that it no longer has.
	if _, err := f.Apply(t.Context(), host.ApplyRequest{Release: host.ReleaseRef{Namespace: testNamespace, Name: "old"}, Owner: host.StackOwner(testNamespace, "shop")}); err != nil {
		t.Fatal(err)
	}

	s, diags := e.LoadStack(t.Context(), stackFiles(twoReleases), "s")
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if _, err := e.DestroyStack(t.Context(), s); err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(f.removed, " "); got != "old app db" {
		t.Errorf("removed %s, want old, then app, then db", got)
	}
}

// noPanic fails the test when fn panics: a panic of the engine ends the
// process of a server that embeds it.
func noPanic(t *testing.T, fn func()) {
	t.Helper()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic: %v", r)
		}
	}()

	fn()
}

// sensitiveStack is a stack of the module db, with the variables and the
// secrets given.
func sensitiveStack(stack, variables, secrets string) fstest.MapFS {
	files := stackFiles("rigger {\n  name      = \"shop\"\n  namespace = \"apps\"\n}\n\n" + stack)
	files["s/variables.hcl"] = &fstest.MapFile{Data: []byte(variables)}
	files["s/secrets.hcl"] = &fstest.MapFile{Data: []byte(secrets)}

	return files
}

// A value that is sensitive as a whole is taken where its parts may be
// sensitive: the values of a release and the items of a secret.
func TestPlanStack_SensitiveObjects(t *testing.T) {
	t.Parallel()

	fsys := sensitiveStack(
		"module \"db\" {\n  source = \"./modules/db\"\n  values = var.db\n}\n",
		"variable \"db\" {\n  sensitive = true\n  default   = { size = 2, password = \"pw\" }\n}\n",
		"secret \"apps/db\" {\n  items = var.db\n}\n",
	)

	noPanic(t, func() {
		f := newFakeCluster()
		e := stackEngine(t, f)

		if _, diags := e.ApplyStack(t.Context(), planOf(t, e, fsys, nil), rigger.ApplyOptions{}); diags.HasErrors() {
			t.Fatal(diags)
		}

		db := f.releases[host.ReleaseRef{Namespace: testNamespace, Name: "db"}][0]
		if len(db.Values) != 0 {
			t.Errorf("the revision records %v, want none of a sensitive object", db.Values)
		}

		size, password := db.Secrets["db"]["size"], db.Secrets["db"]["password"]
		if size.Reveal() != "2" || password.Reveal() != "pw" {
			t.Errorf("secret items = %q, %q, want 2 and pw", size.Reveal(), password.Reveal())
		}

		if got := f.secrets["apps/db"]["password"]; got.Reveal() != "pw" {
			t.Errorf("the secret holds %q, want pw", got.Reveal())
		}
	})
}

// A sensitive value where none may be is an error that says so.
func TestPlanStack_SensitiveRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		stack     string
		variables string
		onceFile  string
		module    map[string]string
		expected  string
	}{
		{
			name:      "overwrite of a secret",
			variables: "variable \"b\" {\n  sensitive = true\n  default   = true\n}\n",
			onceFile:  "secret \"apps/db\" {\n  items     = { a = \"x\" }\n  overwrite = var.b\n}\n",
			expected:  "A sensitive value can't be used in overwrite of a secret",
		},
		{
			name:     "item_of",
			stack:    "module \"db\" {\n  source = \"./modules/db\"\n  values = { l = [\"!generate:g\"] }\n}\n",
			module:   map[string]string{variablesFile: "variable \"l\" {\n  type = list(string)\n\n  validation {\n    item_of = [\"x\"]\n  }\n}\n"},
			expected: "not one of",
		},
		{
			name:  "hook",
			stack: "module \"db\" {\n  source = \"./modules/db\"\n}\n",
			module: map[string]string{
				variablesFile:     "variable \"on\" {\n  sensitive = true\n  default   = \"test\"\n}\n",
				"check.norca.hcl": "job \"check\" {\n  hook {\n    on = var.on\n  }\n}\n",
			},
			expected: "A sensitive value can't be used in a hook",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fsys := sensitiveStack(tt.stack, tt.variables, tt.onceFile)

			if tt.module != nil {
				delete(fsys, "s/modules/db/db.var.hcl")

				for name, src := range tt.module {
					fsys["s/modules/db/"+name] = &fstest.MapFile{Data: []byte(src)}
				}
			}

			noPanic(t, func() {
				e := stackEngine(t, newFakeCluster())

				s, diags := e.LoadStack(t.Context(), fsys, "s")
				if diags.HasErrors() {
					t.Fatal(diags)
				}

				if _, diags := e.PlanStack(t.Context(), s, nil); !strings.Contains(diags.Error(), tt.expected) {
					t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), tt.expected)
				}
			})
		})
	}
}

// A validation is bounded in time as a plan is (Limits.Duration): the host
// that validates the stack of someone else must not wait on it for ever.
func TestValidateStack_Duration(t *testing.T) {
	t.Parallel()

	e := shortEngine(t, rigger.WithJobDecoder(slowJobs{}))

	// The decoder of the jobs takes longer than the bound for each of the two.
	files := stackFiles("rigger {\n  name      = \"shop\"\n  region    = \"eu\"\n  namespace = \"apps\"\n}\n\nmodule \"slow\" {\n  source = \"./modules/slow\"\n}\n")
	files["s/modules/slow/metadata.hcl"] = &fstest.MapFile{Data: []byte("module {\n  name    = \"slow\"\n  version = \"0.1.0\"\n}\n")}
	files["s/modules/slow/a.norca.hcl"] = &fstest.MapFile{Data: []byte("job \"a\" {\n}\n")}
	files["s/modules/slow/b.norca.hcl"] = &fstest.MapFile{Data: []byte("job \"b\" {\n}\n")}

	s, diags := e.LoadStack(t.Context(), files, "s")
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	diags = e.ValidateStack(t.Context(), s, nil)
	if !strings.Contains(diags.Error(), "Evaluation stopped") {
		t.Errorf("diagnostics = %q, want the evaluation stopped", diags.Error())
	}
}

// appRelease is the release app of twoReleases, and the name of its module.
const appRelease = "app"

// disableApp is twoReleases with the release app disabled.
func disableApp(stack string) string {
	return strings.Replace(stack, "source = \"./modules/app\"", "source = \"./modules/app\"\n  enabled = false", 1)
}

// planWarnings plans the stack and returns the plan and the texts of its
// warnings.
func planWarnings(t *testing.T, e *rigger.Engine, fsys fstest.MapFS) (*rigger.StackPlan, []string) {
	t.Helper()

	s, diags := e.LoadStack(t.Context(), fsys, "s")
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	plan, diags := e.PlanStack(t.Context(), s, nil)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	var warnings []string

	for _, d := range diags {
		if d.Severity == hcl.DiagWarning {
			warnings = append(warnings, d.Summary+": "+d.Detail)
		}
	}

	return plan, warnings
}

// A release that is installed and then disabled stays installed (decided by
// the maintainer, 2026-11-24): it is in the plan once, as disabled with the
// revision that stays, an apply with prune does not remove it, and the plan
// has no changes.
func TestStack_DisabledStaysInstalled(t *testing.T) {
	t.Parallel()

	f := newFakeCluster()
	e := stackEngine(t, f)

	if _, diags := e.ApplyStack(t.Context(), planOf(t, e, stackFiles(twoReleases), nil), rigger.ApplyOptions{}); diags.HasErrors() {
		t.Fatal(diags)
	}

	plan, warnings := planWarnings(t, e, stackFiles(disableApp(twoReleases)))

	if got := actions(plan); got != "app:disabled db:unchanged" && got != "db:unchanged app:disabled" {
		t.Fatalf("plan = %s, want app disabled once and db unchanged", got)
	}

	if plan.HasChanges() {
		t.Errorf("the plan has changes: %s", actions(plan))
	}

	for _, r := range plan.Releases {
		if r.Release.Name == appRelease && (r.Revision != 1 || r.Module.Name != appRelease) {
			t.Errorf("the disabled release = revision %d of %q, want the installed revision 1 of app", r.Revision, r.Module.Name)
		}
	}

	if len(warnings) != 0 {
		t.Errorf("warnings = %q, want none: the module is the one installed", warnings)
	}

	if _, diags := e.ApplyStack(t.Context(), plan, rigger.ApplyOptions{Prune: true}); diags.HasErrors() {
		t.Fatal(diags)
	}

	if len(f.removed) != 0 || len(f.releases) != 2 {
		t.Errorf("removed = %v, releases = %d; want nothing removed", f.removed, len(f.releases))
	}
}

// A disabled release that is installed is not updated: the plan warns when
// the stack has another version of its module than the one installed.
func TestStack_DisabledInstalledWarnsOfNewVersion(t *testing.T) {
	t.Parallel()

	f := newFakeCluster()
	e := stackEngine(t, f)

	if _, diags := e.ApplyStack(t.Context(), planOf(t, e, stackFiles(twoReleases), nil), rigger.ApplyOptions{}); diags.HasErrors() {
		t.Fatal(diags)
	}

	files := stackFiles(disableApp(twoReleases))
	files["s/modules/app/metadata.hcl"] = &fstest.MapFile{Data: []byte("module {\n  name    = \"app\"\n  version = \"1.1.0\"\n}\n")}

	plan, warnings := planWarnings(t, e, files)

	if got := strings.Join(warnings, "\n"); len(warnings) != 1 ||
		!strings.Contains(got, appRelease) || !strings.Contains(got, "1.0.0") || !strings.Contains(got, "1.1.0") || !strings.Contains(got, "revision 1") {
		t.Errorf("warnings = %q, want one that names app, the installed 1.0.0 at revision 1 and the 1.1.0 of the stack", warnings)
	}

	if plan.HasChanges() {
		t.Errorf("the plan has changes: %s", actions(plan))
	}
}

// A disabled release that was never installed is only disabled: no revision,
// no warning.
func TestStack_DisabledNotInstalled(t *testing.T) {
	t.Parallel()

	e := stackEngine(t, newFakeCluster())

	plan, warnings := planWarnings(t, e, stackFiles(disableApp(twoReleases)))

	for _, r := range plan.Releases {
		if r.Release.Name == appRelease && (r.Action != rigger.ActionDisabled || r.Revision != 0) {
			t.Errorf("app = %s at revision %d, want disabled and no revision", r.Action, r.Revision)
		}
	}

	if len(warnings) != 0 {
		t.Errorf("warnings = %q, want none", warnings)
	}
}

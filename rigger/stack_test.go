// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
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

	return host.Revision{Release: ref, Number: uint64(len(revs)), Module: last.Module, Digest: last.Digest, Owner: last.Owner}, true
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

	return nil
}

func (f *fakeCluster) Ensure(_ context.Context, s host.Secret) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.secrets[s.Path]; ok && !s.Overwrite {
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
		"s/stack.hcl":                 {Data: []byte(stack)},
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

func TestStack_PlanApply(t *testing.T) {
	t.Parallel()

	f := newFakeCluster()
	e := stackEngine(t, f)
	fsys := stackFiles(twoReleases)

	plan := planOf(t, e, fsys, nil)
	if got := actions(plan); got != "db:install app:install" {
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
	if !strings.Contains(string(app.Resources[0].Spec), `"db":"db-svc"`) || app.Owner != "shop" {
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

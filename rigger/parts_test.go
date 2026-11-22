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
	"github.com/bacanoo-io/norca/rigger/source"
)

// repoFetcher serves one tagged version of a repository tree: every git
// request reads the tree.
type repoFetcher struct {
	tree    fstest.MapFS
	version string
}

const partCommit = "0123456789abcdef0123456789abcdef01234567"

func (f repoFetcher) Versions(_ context.Context, req source.Request) ([]source.Available, error) {
	return []source.Available{{Version: f.version, Ref: req.TagPrefix + f.version, Commit: partCommit}}, nil
}

func (f repoFetcher) Fetch(_ context.Context, req source.Request) (source.Fetched, error) {
	return source.Fetched{
		FS: f.tree, Dir: req.Path,
		Origin: source.Origin{Source: req.Source, Version: req.Version, Ref: req.TagPrefix + req.Version, Commit: partCommit},
	}, nil
}

// publishedVersion is the version of what the test repositories publish.
const publishedVersion = "1.0.0"

// publishedData is a repository with the published stack data 1.0.0: a
// database of its own module, its service as an output.
func publishedData() fstest.MapFS {
	files := stackFiles("")
	tree := fstest.MapFS{
		"stacks/data/metadata.hcl":  {Data: []byte("stack {\n  name    = \"data\"\n  version = \"1.0.0\"\n}\n")},
		"stacks/data/stack.hcl":     {Data: []byte("rigger {\n  name = \"data\"\n}\n\nmodule \"db\" {\n  source = \"./modules/db\"\n  values = { size = var.size, password = \"!generate:pw\" }\n}\n")},
		"stacks/data/variables.hcl": {Data: []byte("variable \"size\" {\n  default = 1\n}\n")},
		"stacks/data/outputs.hcl":   {Data: []byte("output \"service\" {\n  value = module.db.service\n}\n")},
	}

	for name, f := range files {
		if rest, ok := strings.CutPrefix(name, "s/modules/db/"); ok {
			tree["stacks/data/modules/db/"+rest] = f
		}
	}

	return tree
}

// siteStack is a local stack over the part data.
const siteStack = `rigger {
  name = "site"

  registry "acme" {
    source = "github.com/acme/rigger"
  }
}

stack "data" {
  source  = "acme/data"
  version = "~> 1.0"
  values  = { size = 2 }
}

module "app" {
  source = "./modules/app"
  values = { db = stack.data.service }
}
`

func TestStack_Parts(t *testing.T) {
	t.Parallel()

	cluster := newFakeCluster()

	e, err := rigger.New(rigger.WithReleases(cluster), rigger.WithSecrets(cluster), rigger.WithLogger(slog.New(slog.DiscardHandler)),
		rigger.WithFetcher(repoFetcher{tree: publishedData(), version: publishedVersion}))
	if err != nil {
		t.Fatal(err)
	}

	files := stackFiles(siteStack)
	files["s/outputs.hcl"] = &fstest.MapFile{Data: []byte("output \"db\" {\n  value = stack.data.service\n}\n")}

	_, lock, diags := e.SyncStack(t.Context(), files, "s", false)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	for _, want := range []string{`stack "acme/data"`, `version    = "1.0.0"`, `path       = "stacks/data"`, `digest`} {
		if !strings.Contains(string(lock), want) {
			t.Fatalf("lock does not hold %s:\n%s", want, lock)
		}
	}

	files["s/rigger.lock.hcl"] = &fstest.MapFile{Data: lock}

	plan := planOf(t, e, files, nil)

	if got := actions(plan); got != "data-db:install app:install" {
		t.Fatalf("actions = %s", got)
	}

	if deps := plan.Releases[1].DependsOn; len(deps) != 1 || deps[0] != "data-db" {
		t.Fatalf("app depends on %v", deps)
	}

	if len(plan.Generate) != 2 || plan.Generate[0] != "data-pw" {
		t.Fatalf("generate = %v", plan.Generate)
	}

	if len(plan.Outputs) != 1 || string(plan.Outputs[0].Value) != `"data-db-svc"` {
		t.Fatalf("outputs = %+v", plan.Outputs)
	}

	// The part is pinned: other content than the lock records is refused.
	tampered := publishedData()
	tampered["stacks/data/outputs.hcl"] = &fstest.MapFile{Data: []byte("output \"service\" {\n  value = \"x\"\n}\n")}

	e2, _ := rigger.New(rigger.WithLogger(slog.New(slog.DiscardHandler)), rigger.WithFetcher(repoFetcher{tree: tampered, version: publishedVersion}))
	if _, diags := e2.LoadStack(t.Context(), files, "s"); !strings.Contains(diags.Error(), "is not what the lock file records") {
		t.Fatalf("a changed part: %v", diags)
	}
}

// The values of a part may be sensitive as a whole, as those of a release.
func TestPlanStack_SensitivePartValues(t *testing.T) {
	t.Parallel()

	noPanic(t, func() {
		f := newFakeCluster()

		e, err := rigger.New(rigger.WithReleases(f), rigger.WithSecrets(f), rigger.WithLogger(slog.New(slog.DiscardHandler)),
			rigger.WithFetcher(repoFetcher{tree: publishedData(), version: publishedVersion}))
		if err != nil {
			t.Fatal(err)
		}

		files := stackFiles(strings.Replace(siteStack, "values  = { size = 2 }", "values  = var.part", 1))
		files["s/variables.hcl"] = &fstest.MapFile{Data: []byte(
			"variable \"size\" {\n  default = 1\n}\n\nvariable \"part\" {\n  sensitive = true\n  default   = { size = 2 }\n}\n")}

		_, lock, diags := e.SyncStack(t.Context(), files, "s", false)
		if diags.HasErrors() {
			t.Fatal(diags)
		}

		files["s/rigger.lock.hcl"] = &fstest.MapFile{Data: lock}

		if got := actions(planOf(t, e, files, nil)); got != "data-db:install app:install" {
			t.Errorf("actions = %s", got)
		}
	})
}

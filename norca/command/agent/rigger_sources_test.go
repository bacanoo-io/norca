// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/testutil"
	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/bundle"
	"github.com/bacanoo-io/norca/rigger/fetch"
	"github.com/bacanoo-io/norca/rigger/index"
	"github.com/bacanoo-io/norca/rigger/sign"
	"github.com/bacanoo-io/norca/rigger/source"
)

// notRegistered is what the servers say of a source they don't fetch from.
const notRegistered = "the servers fetch only from registered sources"

// testRepo is a git repository of a test, written commit by commit.
type testRepo struct {
	t   *testing.T
	dir string
	r   *git.Repository
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()

	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	must.NoError(t, err)

	return &testRepo{t: t, dir: dir, r: r}
}

// commit writes and commits files and tags the commit.
func (g *testRepo) commit(files map[string]string, tags ...string) {
	g.t.Helper()

	wt, err := g.r.Worktree()
	must.NoError(g.t, err)

	for name, data := range files {
		file := filepath.Join(g.dir, filepath.FromSlash(name))
		must.NoError(g.t, os.MkdirAll(filepath.Dir(file), 0o750))
		must.NoError(g.t, os.WriteFile(file, []byte(data), 0o600))

		_, err := wt.Add(name)
		must.NoError(g.t, err)
	}

	h, err := wt.Commit("change", &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Unix(1700000000, 0)},
	})
	must.NoError(g.t, err)

	for _, tag := range tags {
		_, err := g.r.CreateTag(tag, h, nil)
		must.NoError(g.t, err)
	}
}

// url is the address of the repository in a stack and in a registry.
func (g *testRepo) url() string { return "file://" + filepath.ToSlash(g.dir) }

func greeterRepo(t *testing.T) *testRepo {
	t.Helper()

	g := newTestRepo(t)
	g.commit(map[string]string{
		"modules/greeter/metadata.hcl": "module {\n  name    = \"greeter\"\n  version = \"1.0.0\"\n}\n",
		"modules/greeter/g.var.hcl":    "variable \"g\" {\n  path = \"greeter/${rigger.release}\"\n\n  items {\n    text = \"hello\"\n  }\n}\n",
	}, "greeter/v1.0.0")

	return g
}

func publishedSite(version string) map[string]string {
	return map[string]string{
		"stacks/site/metadata.hcl":               "stack {\n  name    = \"site\"\n  version = \"" + version + "\"\n}\n",
		"stacks/site/stack.hcl":                  "rigger {\n  name      = \"site\"\n  region    = \"global\"\n  namespace = \"default\"\n}\n\nmodule \"front\" {\n  source = \"./modules/front\"\n}\n",
		"stacks/site/modules/front/x.var.hcl":    "variable \"x\" {\n  path = \"site/front\"\n\n  items {\n    version = \"" + version + "\"\n  }\n}\n",
		"stacks/site/modules/front/metadata.hcl": "module {\n  name    = \"front\"\n  version = \"1.0.0\"\n}\n",
	}
}

// anyFetcher serves a module for a source a test can't have (an OCI
// registry), to make the lock file of a stack of it.
type anyFetcher struct{}

func (anyFetcher) Versions(context.Context, source.Request) ([]source.Available, error) {
	return []source.Available{{Version: "1.0.0", Ref: "1.0.0"}}, nil
}

func (anyFetcher) Fetch(_ context.Context, req source.Request) (source.Fetched, error) {
	return source.Fetched{
		FS:     fstest.MapFS{"metadata.hcl": {Data: []byte("module {\n  name    = \"any\"\n  version = \"1.0.0\"\n}\n")}},
		Dir:    ".",
		Origin: source.Origin{Source: req.Source, Version: "1.0.0", Ref: "1.0.0", Commit: "sha256:" + strings.Repeat("a", 64)},
	}, nil
}

// lockedStack is a stack of one module of address with its lock file, as
// rigger stack sync writes it, and its bundle.
func lockedStack(t *testing.T, fetcher source.Fetcher, name, address string) (fstest.MapFS, *rigger.Stack) {
	t.Helper()

	files := fstest.MapFS{"s/stack.hcl": {Data: []byte("rigger {\n  name      = \"" + name + "\"\n  region    = \"global\"\n  namespace = \"default\"\n}\n\nmodule \"" + name + "-mod\" {\n  source = \"" + address + "\"\n}\n")}}

	eng, err := rigger.New(rigger.WithFetcher(fetcher))
	must.NoError(t, err)

	_, lock, diags := eng.SyncStack(t.Context(), files, "s", false)
	must.False(t, diags.HasErrors(), must.Sprint(diags))

	files["s/rigger.lock.hcl"] = &fstest.MapFile{Data: lock}

	st, diags := eng.LoadStack(t.Context(), files, "s")
	must.False(t, diags.HasErrors(), must.Sprint(diags))

	return files, st
}

func sentStack(t *testing.T, fetcher source.Fetcher, name, address string) *api.RiggerStackRequest {
	t.Helper()

	_, st := lockedStack(t, fetcher, name, address)

	archive, err := st.Archive()
	must.NoError(t, err)

	return &api.RiggerStackRequest{Files: archive}
}

func applyTestRegistry(t *testing.T, client *api.Client, name string, spec map[string]any) {
	t.Helper()

	obj := &api.CustomObject{Group: api.RiggerObjectGroup, Kind: api.RiggerRegistryKind, Name: name, Spec: spec}

	must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
		_, _, err := client.Objects().Apply(obj, nil, nil)

		return err
	}), wait.Timeout(30*time.Second), wait.Gap(100*time.Millisecond)))
}

func syncTestRegistry(t *testing.T, client *api.Client, name string) {
	t.Helper()

	_, _, err := client.Rigger().SyncRegistry(name, nil)
	must.NoError(t, err)
}

func testPublicKey(t *testing.T) string {
	t.Helper()

	key, err := sign.GenerateKey()
	must.NoError(t, err)

	pub, err := sign.MarshalPublicKey(&key.PublicKey)
	must.NoError(t, err)

	return string(pub)
}

// The servers fetch only from builtin/, the synced global registry and
// registered git registries, at the commit of their sync and with their
// keys, for a stack sent as files and a published one: no directory of the
// servers, no OCI registry, no repository a request names.
func TestHTTP_RiggerSources(t *testing.T) {
	ci.Parallel(t)

	catalog, modules := newTestRepo(t), greeterRepo(t)
	catalog.commit(publishedSite("1.0.0"), "site/v1.0.0")

	dir := t.TempDir()
	must.NoError(t, os.WriteFile(filepath.Join(dir, "metadata.hcl"), []byte("module {\n  name    = \"secret\"\n  version = \"1.0.0\"\n}\n"), 0o600))

	local := func() source.Fetcher { return fetch.New(t.TempDir()) }
	published := &api.RiggerStackRequest{Source: catalog.url() + "//stacks/site"}
	sent := sentStack(t, local(), "greet", modules.url()+"//modules/greeter")

	refused := map[string]*api.RiggerStackRequest{
		"a directory":       sentStack(t, local(), "dirs", "dir://"+filepath.ToSlash(dir)),
		"an OCI registry":   sentStack(t, anyFetcher{}, "ocis", "oci://127.0.0.1:1/acme/any"),
		"a git repository":  sent,
		"a published stack": published,
	}

	httpACLTest(t, nil, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.RPC)

		client := s.Client()
		client.SetSecretID(s.RootToken.SecretID)

		for name, req := range refused {
			_, _, err := client.Rigger().StackPlan(req, nil)
			must.ErrorContains(t, err, notRegistered, must.Sprint(name))
			must.ErrorContains(t, err, "400", must.Sprint(name))
		}

		applyTestRegistry(t, client, "catalog", map[string]any{"type": "git", "source": catalog.url()})

		_, _, err := client.Rigger().StackPlan(published, nil)
		must.ErrorContains(t, err, "has not been synced")

		syncTestRegistry(t, client, "catalog")

		op, _, err := client.Rigger().StackApply(published, nil)
		must.NoError(t, err)

		op = waitOperation(t, client, op.Name)
		must.Eq(t, "Succeeded", op.Status.Phase, must.Sprint(op.Status.Error))

		rel, _, err := client.Releases().Info("front", nil)
		must.NoError(t, err)
		must.True(t, rel.Spec.Pack.ServerRendered, must.Sprint("a stack of a synced registry is rendered by the server"))

		// A version tagged after the sync is offered after the next one.
		catalog.commit(publishedSite("1.1.0"), "site/v1.1.0")

		newer := &api.RiggerStackRequest{Source: published.Source, Version: ">= 1.1.0"}
		_, _, err = client.Rigger().StackPlan(newer, nil)
		must.ErrorContains(t, err, "no version meets")

		syncTestRegistry(t, client, "catalog")

		_, _, err = client.Rigger().StackPlan(newer, nil)
		must.NoError(t, err)

		applyTestRegistry(t, client, "modules", map[string]any{"type": "git", "source": modules.url()})
		syncTestRegistry(t, client, "modules")

		_, _, err = client.Rigger().StackPlan(sent, nil)
		must.NoError(t, err)

		// The modules of a registry need read on the registries.
		releasesOnly := mock.CreatePolicyAndToken(t, s.Agent.Server().State(), 1700, "sources-releases-only",
			`namespace "default" { object "rigger.norca.io/Release" { capabilities = ["read", "write"] } }`)

		limited := s.Client()
		limited.SetSecretID(releasesOnly.SecretID)

		_, _, err = limited.Rigger().StackPlan(sent, nil)
		must.ErrorContains(t, err, "Permission denied")

		// The keys of a registry hold for whoever names its repository.
		applyTestRegistry(t, client, "strict", map[string]any{"type": "git", "source": modules.url(), "trustedKeys": []any{testPublicKey(t)}})
		syncTestRegistry(t, client, "strict")

		_, _, err = client.Rigger().StackPlan(sent, nil)
		must.ErrorContains(t, err, rigger.ErrNotTrusted.Error())

		applyTestRegistry(t, client, "catalog-strict", map[string]any{"type": "git", "source": catalog.url(), "trustedKeys": []any{testPublicKey(t)}})
		syncTestRegistry(t, client, "catalog-strict")

		_, _, err = client.Rigger().StackPlan(published, nil)
		must.ErrorContains(t, err, rigger.ErrNotTrusted.Error())
	})
}

// An entry of the global registry names no path of the servers.
func TestHTTP_RiggerSources_GlobalLocalPath(t *testing.T) {
	ci.Parallel(t)

	modules, idx := greeterRepo(t), newTestRepo(t)

	head, err := modules.r.Head()
	must.NoError(t, err)

	entry, err := index.Marshal(index.Entry{Namespace: "acme", Name: "greeter", Versions: []index.Version{{
		Version: "1.0.0", Repository: modules.url(), Path: "modules/greeter", Tag: "greeter/v1.0.0",
		Commit: head.Hash().String(), Digest: "sha256:" + strings.Repeat("b", 64),
		Signatures: []index.Signature{{KeyID: "sha256:" + strings.Repeat("c", 64), Signature: "MEUCIQ=="}}, Listed: "2026-11-22",
	}}})
	must.NoError(t, err)

	idx.commit(map[string]string{index.EntryPath(index.KindModule, "acme", "greeter"): string(entry)})

	httpACLTest(t, nil, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.RPC)

		client := s.Client()
		client.SetSecretID(s.RootToken.SecretID)

		applyTestRegistry(t, client, "global", map[string]any{"type": "global", "source": idx.url()})
		syncTestRegistry(t, client, "global")

		_, _, err := client.Rigger().Install(&api.RiggerInstallRequest{Release: "g", Registry: "global", Module: "acme/greeter"}, true, nil)
		must.ErrorContains(t, err, "is a path on the servers")
	})
}

// An imported bundle is a source for what a lock file pins by repository,
// commit and digest, while it is imported; what came from one without an
// index is not rendered by the server.
func TestHTTP_RiggerSources_Bundle(t *testing.T) {
	ci.Parallel(t)

	catalog, modules := newTestRepo(t), greeterRepo(t)
	files, st := lockedStack(t, fetch.New(t.TempDir()), "bundled", modules.url()+"//modules/greeter")

	var bundled bytes.Buffer
	must.NoError(t, bundle.Write(&bundled, st.Name(), st.SourcedModules()))

	catalog.commit(map[string]string{
		"stacks/bundled/metadata.hcl":    "stack {\n  name    = \"bundled\"\n  version = \"1.0.0\"\n}\n",
		"stacks/bundled/stack.hcl":       string(files["s/stack.hcl"].Data),
		"stacks/bundled/rigger.lock.hcl": string(files["s/rigger.lock.hcl"].Data),
	}, "bundled/v1.0.0")

	withBus := func(c *Config) { c.Server.Bus = &BusConfig{Enabled: pointer.Of(true)} }

	httpACLTest(t, withBus, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.RPC)

		client := s.Client()
		client.SetSecretID(s.RootToken.SecretID)

		applyTestRegistry(t, client, "catalog", map[string]any{"type": "git", "source": catalog.url()})
		syncTestRegistry(t, client, "catalog")

		req := &api.RiggerStackRequest{Source: catalog.url() + "//stacks/bundled"}

		_, _, err := client.Rigger().StackPlan(req, nil)
		must.ErrorContains(t, err, notRegistered)

		var b *api.RiggerBundle

		must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
			var err error
			b, _, err = client.Rigger().ImportBundle(bundled.Bytes(), nil)

			return err
		}), wait.Timeout(30*time.Second), wait.Gap(200*time.Millisecond)))

		op, _, err := client.Rigger().StackApply(req, nil)
		must.NoError(t, err)

		op = waitOperation(t, client, op.Name)
		must.Eq(t, "Succeeded", op.Status.Phase, must.Sprint(op.Status.Error))

		rel, _, err := client.Releases().Info("bundled-mod", nil)
		must.NoError(t, err)
		must.False(t, rel.Spec.Pack.ServerRendered, must.Sprint("a module of a bundle alone is no module of the catalog of the servers"))

		_, err = client.Rigger().DeleteBundle(b.Name, nil)
		must.NoError(t, err)

		_, _, err = client.Rigger().StackPlan(req, nil)
		must.ErrorContains(t, err, notRegistered, must.Sprint("the bundle is gone, its tree in the cache is no source"))
	})
}

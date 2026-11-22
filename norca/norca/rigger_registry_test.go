// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/bacanoo-io/norca/rigger/catalog"
	"github.com/bacanoo-io/norca/rigger/fetch"
	"github.com/bacanoo-io/norca/rigger/index"
)

// registryTest drives the registries of Rigger on a test server with ACLs.
type registryTest struct {
	t     *testing.T
	s     *Server
	token string
}

func newRegistryTest(t *testing.T) *registryTest {
	t.Helper()

	s, root, cleanup := TestACLServer(t, nil)
	t.Cleanup(cleanup)
	testutil.WaitForLeader(t, s.RPC)
	waitOperatorKinds(t, s)

	return &registryTest{t: t, s: s, token: root.SecretID}
}

func (r *registryTest) as(token string) *registryTest {
	return &registryTest{t: r.t, s: r.s, token: token}
}

func (r *registryTest) apply(name string, spec map[string]any) error {
	var resp structs.CustomObjectWriteResponse

	return r.s.RPC("CustomObject.Apply", &structs.CustomObjectApplyRequest{
		Object:       &structs.CustomObject{Group: structs.RiggerObjectGroup, Kind: structs.RiggerRegistryKind, Name: name, Spec: spec},
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: r.token},
	}, &resp)
}

func (r *registryTest) sync(name string) (*RiggerRegistry, error) {
	var resp RiggerRegistryResponse
	err := r.s.RPC("Rigger.SyncRegistry", &RiggerRegistryNameRequest{
		Name: name, WriteRequest: structs.WriteRequest{Region: "global", AuthToken: r.token},
	}, &resp)

	return resp.Registry, err
}

func (r *registryTest) search(name string, q RiggerRegistrySearchRequest) (*RiggerRegistrySearchResponse, error) {
	q.Name = name
	q.QueryOptions = structs.QueryOptions{Region: "global", AuthToken: r.token}

	var resp RiggerRegistrySearchResponse
	err := r.s.RPC("Rigger.SearchRegistry", &q, &resp)

	return &resp, err
}

func (r *registryTest) get(name string) *RiggerRegistry {
	r.t.Helper()

	obj, err := r.s.State().CustomObjectByName(nil, structs.RiggerObjectGroup, structs.RiggerRegistryKind, "", name)
	must.NoError(r.t, err)
	must.NotNil(r.t, obj, must.Sprintf("registry %s", name))

	reg, err := riggerRegistryFromObject(obj)
	must.NoError(r.t, err)

	return reg
}

// gitRepo is a git repository in a directory of the test, written commit by
// commit.
type gitRepo struct {
	t   *testing.T
	dir string
	r   *git.Repository
}

func newGitRepo(t *testing.T) *gitRepo {
	t.Helper()

	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	must.NoError(t, err)

	return &gitRepo{t: t, dir: dir, r: r}
}

// commit writes the files and commits them; it returns the commit.
func (g *gitRepo) commit(files map[string]string) plumbing.Hash {
	g.t.Helper()

	wt, err := g.r.Worktree()
	must.NoError(g.t, err)

	for name, data := range files {
		path := filepath.Join(g.dir, filepath.FromSlash(name))
		must.NoError(g.t, os.MkdirAll(filepath.Dir(path), 0o750))
		must.NoError(g.t, os.WriteFile(path, []byte(data), 0o600))
		_, err := wt.Add(name)
		must.NoError(g.t, err)
	}

	h, err := wt.Commit("change", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Unix(1700000000, 0)}})
	must.NoError(g.t, err)

	return h
}

func (g *gitRepo) tag(name string, h plumbing.Hash) {
	g.t.Helper()

	_, err := g.r.CreateTag(name, h, nil)
	must.NoError(g.t, err)
}

// registryRepo is a registry repository: the module web at 1.0.0 and 1.1.0,
// the stack site at 0.1.0, the module draft without a version.
func registryRepo(t *testing.T) *gitRepo {
	t.Helper()

	g := newGitRepo(t)
	first := g.commit(map[string]string{"modules/web/metadata.hcl": "module {}\n", "stacks/site/metadata.hcl": "stack {}\n"})
	g.tag("web/v1.0.0", first)
	g.tag("site/v0.1.0", first)
	g.tag("web/v1.1.0", g.commit(map[string]string{"modules/draft/metadata.hcl": "module {}\n"}))

	return g
}

// globalRepo is an index of the global registry: the module norca/nats and
// the stack norca/site.
func globalRepo(t *testing.T) *gitRepo {
	t.Helper()

	entry := func(name string, versions ...string) string {
		e := index.Entry{Namespace: "norca", Name: name}
		for _, v := range versions {
			e.Versions = append(e.Versions, index.Version{
				Version: v, Repository: "github.com/norca/rigger", Path: "modules/" + name, Tag: name + "/v" + v,
				Commit: strings.Repeat("a", 40), Digest: "sha256:" + strings.Repeat("b", 64),
				Signatures: []index.Signature{{KeyID: "sha256:" + strings.Repeat("c", 64), Signature: "MEUCIQ=="}}, Listed: "2026-11-22",
			})
		}

		data, err := index.Marshal(e)
		must.NoError(t, err)

		return string(data)
	}

	g := newGitRepo(t)
	g.commit(map[string]string{
		index.EntryPath(index.KindModule, "norca", "nats"): entry("nats", "1.0.0", "1.1.0"),
		index.EntryPath(index.KindStack, "norca", "site"):  entry("site", "0.1.0"),
	})

	return g
}

func describeItems(items []catalog.Item) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, strings.TrimSpace(item.Ref.String()+" "+strings.Join(item.Versions, " ")))
	}

	return strings.Join(parts, "; ")
}

// The leader creates the registry global, shown in the UI only with what is
// selected and not synced: it offers nothing until someone syncs it.
func TestRiggerRegistry_Global(t *testing.T) {
	ci.Parallel(t)

	r := newRegistryTest(t)

	var reg *RiggerRegistry

	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		obj, err := r.s.State().CustomObjectByName(nil, structs.RiggerObjectGroup, structs.RiggerRegistryKind, "", structs.RiggerGlobalRegistry)
		if err != nil || obj == nil {
			return false
		}

		reg, err = riggerRegistryFromObject(obj)

		return err == nil
	}), wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond)), must.Sprint("the registry global is created"))

	must.Eq(t, RiggerRegistrySpec{Type: riggerRegistryTypeGlobal, Source: fetch.DefaultGlobalRegistry, Display: riggerDisplaySelected}, reg.Spec)
	must.Eq(t, "", reg.Status.Commit)

	_, err := r.search(structs.RiggerGlobalRegistry, RiggerRegistrySearchRequest{})
	must.ErrorContains(t, err, "has not been synced: sync it first (rigger registry sync global)")
}

// A registry repository: a sync reads its modules and stacks with the
// versions of its tags at the head; a search pages through them. A tag made
// after the sync, on a later commit, is offered only after the next sync.
func TestRiggerRegistry_SyncAndSearchRepository(t *testing.T) {
	ci.Parallel(t)

	r := newRegistryTest(t)
	repo := registryRepo(t)

	must.NoError(t, r.apply("acme", map[string]any{"type": "git", "source": repo.dir}))

	reg, err := r.sync("acme")
	must.NoError(t, err)
	must.Eq(t, riggerRegistryReady, reg.Status.Phase)
	must.Eq(t, 2, reg.Status.Modules)
	must.Eq(t, 1, reg.Status.Stacks)
	must.Eq(t, riggerDisplayCatalog, reg.Spec.Display)
	must.Nil(t, reg.Status.Shown, must.Sprint("a registry shown whole lists nothing in its status"))

	all, err := r.search("acme", RiggerRegistrySearchRequest{})
	must.NoError(t, err)
	must.Eq(t, "module draft; module web 1.1.0 1.0.0; stack site 0.1.0", describeItems(all.Items))
	must.Eq(t, reg.Status.Commit, all.Commit)

	page, err := r.search("acme", RiggerRegistrySearchRequest{Kind: "module", Text: "WE", Limit: 1})
	must.NoError(t, err)
	must.Eq(t, "module web 1.1.0 1.0.0", describeItems(page.Items))
	must.Eq(t, 1, page.Total)

	_, err = r.search("acme", RiggerRegistrySearchRequest{Kind: "policy"})
	must.ErrorContains(t, err, "invalid query")

	// A new version: not offered until the next sync.
	repo.tag("web/v1.2.0", repo.commit(map[string]string{"modules/web/README.md": "web\n"}))

	before, err := r.search("acme", RiggerRegistrySearchRequest{Text: "web"})
	must.NoError(t, err)
	must.Eq(t, "module web 1.1.0 1.0.0", describeItems(before.Items))

	_, err = r.sync("acme")
	must.NoError(t, err)

	after, err := r.search("acme", RiggerRegistrySearchRequest{Text: "web"})
	must.NoError(t, err)
	must.Eq(t, "module web 1.2.0 1.1.0 1.0.0", describeItems(after.Items))
}

// The global registry, pointed at a mirror, shows what is selected: the
// sync resolves the selection and reports what the index lacks; a change of
// the selection reaches the status only with the next sync.
func TestRiggerRegistry_GlobalSelected(t *testing.T) {
	ci.Parallel(t)

	r := newRegistryTest(t)
	repo := globalRepo(t)

	spec := map[string]any{
		"type": "global", "source": repo.dir, "display": "selected",
		"selected": []any{
			map[string]any{"kind": "module", "namespace": "norca", "name": "nats"},
			map[string]any{"kind": "module", "namespace": "acme", "name": "gone"},
		},
	}
	must.NoError(t, r.apply(structs.RiggerGlobalRegistry, spec))

	reg, err := r.sync(structs.RiggerGlobalRegistry)
	must.NoError(t, err)
	must.Eq(t, 1, reg.Status.Modules)
	must.Eq(t, 1, reg.Status.Stacks)
	must.Eq(t, "module norca/nats 1.1.0 1.0.0", describeItems(reg.Status.Shown))
	must.Eq(t, []catalog.Ref{{Kind: index.KindModule, Namespace: "acme", Name: "gone"}}, reg.Status.Missing)

	// The selection changes the UI only: a search finds everything.
	all, err := r.search(structs.RiggerGlobalRegistry, RiggerRegistrySearchRequest{})
	must.NoError(t, err)
	must.Eq(t, 2, all.Total)

	// A new selection: the status keeps the old one until a sync.
	spec["selected"] = []any{map[string]any{"kind": "stack", "namespace": "norca", "name": "site"}}
	must.NoError(t, r.apply(structs.RiggerGlobalRegistry, spec))
	must.Eq(t, "module norca/nats 1.1.0 1.0.0", describeItems(r.get(structs.RiggerGlobalRegistry).Status.Shown))

	reg, err = r.sync(structs.RiggerGlobalRegistry)
	must.NoError(t, err)
	must.Eq(t, "stack norca/site 0.1.0", describeItems(reg.Status.Shown))
	must.Nil(t, reg.Status.Missing)
}

// A sync that fails is written as the phase Error with its message, keeps
// what the last good sync found, and is an error of the call.
func TestRiggerRegistry_FailedSync(t *testing.T) {
	ci.Parallel(t)

	r := newRegistryTest(t)
	repo := registryRepo(t)

	must.NoError(t, r.apply("acme", map[string]any{"type": "git", "source": repo.dir}))

	good, err := r.sync("acme")
	must.NoError(t, err)

	gone := filepath.Join(t.TempDir(), "gone")
	must.NoError(t, r.apply("acme", map[string]any{"type": "git", "source": gone}))

	_, err = r.sync("acme")
	must.ErrorContains(t, err, `sync of the registry "acme"`)

	failed := r.get("acme")
	must.Eq(t, riggerRegistryError, failed.Status.Phase)
	must.StrContains(t, failed.Status.Message, gone)
	must.Eq(t, good.Status.Commit, failed.Status.Commit)
	must.Eq(t, good.Status.LastGoodSyncAt, failed.Status.LastGoodSyncAt)

	// The last good sync still serves a search, from its own source.
	all, err := r.search("acme", RiggerRegistrySearchRequest{})
	must.NoError(t, err)
	must.Eq(t, 3, all.Total)

	_, err = r.sync("missing")
	must.ErrorContains(t, err, "not found")
}

// The checks of a write and of the calls: the type global is the registry
// global's only, selected names fit the type, the status is the servers',
// an authSecret needs read on its Variable, a sync needs write and a search
// read on the kind.
func TestRiggerRegistry_Checks(t *testing.T) {
	ci.Parallel(t)

	r := newRegistryTest(t)
	repo := registryRepo(t)

	tests := []struct {
		name string
		reg  string
		spec map[string]any
		err  string
	}{
		{name: "global type elsewhere", reg: "acme", spec: map[string]any{"type": "global", "source": repo.dir}, err: `only the registry "global"`},
		{name: "git type for global", reg: "global", spec: map[string]any{"type": "git", "source": repo.dir}, err: `only the registry "global"`},
		{name: "namespace in a repository", reg: "acme", spec: map[string]any{"type": "git", "source": repo.dir, "selected": []any{map[string]any{"kind": "module", "namespace": "x", "name": "web"}}}, err: "has no namespaces"},
		{name: "unknown display", reg: "acme", spec: map[string]any{"type": "git", "source": repo.dir, "display": "all"}, err: "display"},
		{name: "missing Variable", reg: "acme", spec: map[string]any{"type": "git", "source": "https://example.com/r", "authSecret": map[string]any{"path": "creds/none"}}, err: "does not exist"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := r.apply(tt.reg, tt.spec)
			must.ErrorContains(t, err, tt.err)
		})
	}

	must.NoError(t, r.apply("acme", map[string]any{"type": "git", "source": repo.dir}))

	var resp structs.CustomObjectWriteResponse
	err := r.s.RPC("CustomObject.UpdateStatus", &structs.CustomObjectStatusRequest{
		Group: structs.RiggerObjectGroup, Kind: structs.RiggerRegistryKind, Name: "acme", Status: map[string]any{"commit": "x"},
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: r.token},
	}, &resp)
	must.ErrorContains(t, err, "written by the servers")

	reader := mock.CreatePolicyAndToken(t, r.s.State(), 1001, "registry-read",
		`namespace "*" { object "rigger.norca.io/Registry" { capabilities = ["read"] } }`)
	writer := mock.CreatePolicyAndToken(t, r.s.State(), 1002, "registry-write",
		`namespace "*" { object "rigger.norca.io/Registry" { capabilities = ["read", "write"] } }`)

	_, err = r.as(reader.SecretID).sync("acme")
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())

	_, err = r.as(writer.SecretID).sync("acme")
	must.NoError(t, err)

	_, err = r.as(reader.SecretID).search("acme", RiggerRegistrySearchRequest{})
	must.NoError(t, err)

	// A writer of registries that can't read the Variable of the
	// credentials can't name it.
	err = r.as(writer.SecretID).apply("private", map[string]any{
		"type": "git", "source": "https://example.com/r", "authSecret": map[string]any{"path": "creds/r"},
	})
	must.ErrorContains(t, err, `can't read the Variable "creds/r"`)
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
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
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	rigger "github.com/bacanoo-io/norca/rigger"
	riggercmd "github.com/bacanoo-io/norca/rigger/cli/command"
	"github.com/bacanoo-io/norca/rigger/fetch"
	"github.com/bacanoo-io/norca/rigger/sign"
)

// moduleRegistryRepo is a registry repository with the module of the host
// test (riggerHostModule) in modules/web, tagged web/v1.0.0.
func moduleRegistryRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	must.NoError(t, err)

	wt, err := r.Worktree()
	must.NoError(t, err)

	must.NoError(t, fs.WalkDir(riggerHostModule, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		data, err := fs.ReadFile(riggerHostModule, p)
		if err != nil {
			return err
		}

		file := filepath.Join(dir, "modules", "web", filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
			return err
		}

		if err := os.WriteFile(file, data, 0o600); err != nil {
			return err
		}

		_, err = wt.Add(path.Join("modules", "web", p))

		return err
	}))

	h, err := wt.Commit("web 1.0.0", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Unix(1700000000, 0)}})
	must.NoError(t, err)

	_, err = r.CreateTag("web/v1.0.0", h, nil)
	must.NoError(t, err)

	return dir
}

// One module installed by the server (design/rig-integration.md 4.6a): a
// module of a synced registry is planned (unsigned: a warning) and applied
// as rendered by the server; a second plan is unchanged; Detail answers for
// the form; a constraint no version meets and a registry that requires
// signatures are refused; a local module sent as files is applied as a
// client's, with the source local; a token that can't read registries is
// refused a module of one.
func TestHTTP_RiggerInstall(t *testing.T) {
	ci.Parallel(t)

	repo := moduleRegistryRepo(t)

	httpACLTest(t, nil, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.RPC)

		client := s.Client()
		client.SetSecretID(s.RootToken.SecretID)

		apply := func(name string, spec map[string]any) {
			t.Helper()

			obj := &api.CustomObject{Group: api.RiggerObjectGroup, Kind: api.RiggerRegistryKind, Name: name, Spec: spec}
			must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
				_, _, err := client.Objects().Apply(obj, nil, nil)

				return err
			}), wait.Timeout(30*time.Second), wait.Gap(100*time.Millisecond)))

			_, _, err := client.Rigger().SyncRegistry(name, nil)
			must.NoError(t, err)
		}

		apply("acme", map[string]any{"type": "git", "source": repo})

		req := &api.RiggerInstallRequest{Release: "web", Registry: "acme", Module: "web", Values: "count = 2\n"}

		plan, _, err := client.Rigger().Install(req, true, nil)
		must.NoError(t, err)
		must.Eq(t, "install", plan.Action)
		must.Eq(t, "acme/web", plan.Module.Source)
		must.Eq(t, "1.0.0", plan.Module.Version)
		must.Eq(t, "web/v1.0.0", plan.Origin.Ref)
		must.StrContains(t, strings.Join(plan.Warnings, "; "), "is not signed")

		done, _, err := client.Rigger().Install(req, false, nil)
		must.NoError(t, err)
		must.Eq(t, uint64(1), done.Revision)

		rel, _, err := client.Releases().Info("web", nil)
		must.NoError(t, err)
		must.Eq(t, "rigger", rel.Spec.Pack.Manager)

		var srel structs.SingleReleaseResponse
		must.NoError(t, s.RPC("Release.GetRelease", &structs.ReleaseSpecificRequest{
			Name: "web", QueryOptions: structs.QueryOptions{Region: "global", Namespace: "default", AuthToken: s.RootToken.SecretID},
		}, &srel))
		must.True(t, srel.Release.Spec.Pack.ServerRendered, must.Sprint("a module of a registry is applied as rendered by the server"))
		must.Eq(t, "acme/web", srel.Release.Spec.Pack.Source)
		must.Eq(t, "acme", srel.Release.Spec.Pack.Registry, must.Sprint("the registry of the cluster it came from, for an upgrade"))

		again, _, err := client.Rigger().Install(req, true, nil)
		must.NoError(t, err)
		must.Eq(t, "unchanged", again.Action)

		// The same values as JSON, as the form of the UI sends them: the
		// same release.
		asJSON := &api.RiggerInstallRequest{Release: "web", Registry: "acme", Module: "web", ValuesJSON: json.RawMessage(`{"count": 2}`)}
		again, _, err = client.Rigger().Install(asJSON, true, nil)
		must.NoError(t, err)
		must.Eq(t, "unchanged", again.Action)

		both := *asJSON
		both.Values = "count = 2\n"
		_, _, err = client.Rigger().Install(&both, true, nil)
		must.ErrorContains(t, err, "one of")

		detail, _, err := client.Rigger().ModuleDetail("acme", "web", "", nil)
		must.NoError(t, err)
		must.Eq(t, "web", detail.Metadata["name"])
		must.Eq(t, []string{"1.0.0"}, detail.Versions)
		must.True(t, detail.Meets)
		must.NotNil(t, detail.FormSchema)
		must.False(t, detail.Icon)

		_, err = client.Rigger().Icon("acme", "web", "", nil)
		must.ErrorContains(t, err, "has no icon")

		_, _, err = client.Rigger().Install(&api.RiggerInstallRequest{Release: "web", Registry: "acme", Module: "web", Version: ">= 2.0.0"}, true, nil)
		must.ErrorContains(t, err, `no version meets ">= 2.0.0"`)

		// A registry that trusts a key: an unsigned version is refused.
		key, err := sign.GenerateKey()
		must.NoError(t, err)
		pub, err := sign.MarshalPublicKey(&key.PublicKey)
		must.NoError(t, err)

		apply("signed", map[string]any{"type": "git", "source": repo, "trustedKeys": []any{string(pub)}})

		_, _, err = client.Rigger().Install(&api.RiggerInstallRequest{Release: "web-signed", Registry: "signed", Module: "web"}, true, nil)
		must.ErrorContains(t, err, rigger.ErrNotTrusted.Error())

		// A local module sent as files, with next steps to show after an
		// apply.
		eng, err := rigger.New()
		must.NoError(t, err)

		withNextSteps := maps.Clone(riggerHostModule)
		withNextSteps["outputs.hcl"] = &fstest.MapFile{Data: []byte(
			"output \"next_steps\" {\n  value = \"${rigger.release} runs in ${rigger.namespace}.\"\n}\n")}

		m, diags := eng.LoadModule(t.Context(), withNextSteps)
		must.False(t, diags.HasErrors(), must.Sprint(diags))

		files, err := m.Archive()
		must.NoError(t, err)

		// In a namespace of its own: the job and Variable of the module are
		// the release web's in the namespace default.
		_, err = client.Namespaces().Register(&api.Namespace{Name: "local"}, nil)
		must.NoError(t, err)

		local, _, err := client.Rigger().Install(&api.RiggerInstallRequest{Release: "web-local", Files: files}, false, &api.WriteOptions{Namespace: "local"})
		must.NoError(t, err)
		must.Eq(t, uint64(1), local.Revision)
		must.Eq(t, "local", local.Module.Source)
		must.Nil(t, local.Origin)
		must.Eq(t, "web-local runs in local.", local.NextSteps)

		planned, _, err := client.Rigger().Install(&api.RiggerInstallRequest{Release: "web-local", Files: files}, true, &api.WriteOptions{Namespace: "local"})
		must.NoError(t, err)
		must.Eq(t, "", planned.NextSteps, must.Sprint("a plan shows no next steps"))

		must.NoError(t, s.RPC("Release.GetRelease", &structs.ReleaseSpecificRequest{
			Name: "web-local", QueryOptions: structs.QueryOptions{Region: "global", Namespace: "local", AuthToken: s.RootToken.SecretID},
		}, &srel))
		must.False(t, srel.Release.Spec.Pack.ServerRendered, must.Sprint("a local module is applied as a client's"))
		must.Eq(t, "", srel.Release.Spec.Pack.Registry, must.Sprint("a local module is of no registry"))

		_, _, err = client.Rigger().Install(&api.RiggerInstallRequest{Release: "x", Registry: "acme", Files: files}, true, nil)
		must.ErrorContains(t, err, "without a registry")

		// A token that writes releases but can't read registries.
		releasesOnly := mock.CreatePolicyAndToken(t, s.Agent.Server().State(), 1600, "releases-only",
			`namespace "default" { object "rigger.norca.io/Release" { capabilities = ["read", "write"] } }`)
		client.SetSecretID(releasesOnly.SecretID)

		_, _, err = client.Rigger().Install(&api.RiggerInstallRequest{Release: "web-2", Registry: "acme", Module: "web"}, true, nil)
		must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	})
}

// A module built into the servers (builtin/<name>, no registry) is
// installed by its name: of the one version the servers have, which must
// meet the constraint, with no signature and no registry, so a token that
// writes releases but can't read registries installs it; its release is
// applied as rendered by the server. A module that names no registry and is
// no built-in one, and a built-in module the servers don't have, are
// refused.
func TestHTTP_RiggerInstallBuiltin(t *testing.T) {
	ci.Parallel(t)

	local, err := rigger.New(rigger.WithFetcher(fetch.New(t.TempDir())))
	must.NoError(t, err)

	redis, diags := local.FetchNewestModule(t.Context(), "builtin/redis", "")
	must.False(t, diags.HasErrors(), must.Sprint(diags))

	version := redis.Metadata().Version

	httpACLTest(t, nil, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.RPC)

		client := s.Client()
		client.SetSecretID(s.RootToken.SecretID)

		req := &api.RiggerInstallRequest{Release: "cache", Module: "builtin/redis", Version: ">= " + version}

		var plan *api.RiggerInstallResponse

		// The kinds of Rigger are registered when the leader starts.
		must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
			plan, _, err = client.Rigger().Install(req, true, nil)

			return err
		}), wait.Timeout(30*time.Second), wait.Gap(100*time.Millisecond)))

		must.Eq(t, "install", plan.Action)
		must.Eq(t, "builtin/redis", plan.Module.Source)
		must.Eq(t, version, plan.Module.Version)
		must.NotNil(t, plan.Origin)
		must.Eq(t, "builtin/redis", plan.Origin.Source)
		must.SliceEmpty(t, plan.Origin.Signers)
		must.StrNotContains(t, strings.Join(plan.Warnings, "; "), "not signed")

		_, _, err = client.Rigger().Install(&api.RiggerInstallRequest{Release: "cache", Module: "builtin/redis", Version: ">= 99.0.0"}, true, nil)
		must.ErrorContains(t, err, `no version meets the constraint ">= 99.0.0"`)

		_, _, err = client.Rigger().Install(&api.RiggerInstallRequest{Release: "cache", Module: "builtin/nope"}, true, nil)
		must.ErrorContains(t, err, "the servers have no built-in module builtin/nope")

		_, _, err = client.Rigger().Install(&api.RiggerInstallRequest{Release: "cache", Module: "web"}, true, nil)
		must.ErrorContains(t, err, `the module "web" names no registry`)

		// The release, applied as rendered by the server. The job of Redis
		// needs docker, which the client of the test has not: the wait for
		// it ends with the timeout, the release is written before.
		applied := *req
		applied.Timeout = 2 * time.Second
		_, _, _ = client.Rigger().Install(&applied, false, nil)

		var srel structs.SingleReleaseResponse
		must.NoError(t, s.RPC("Release.GetRelease", &structs.ReleaseSpecificRequest{
			Name: "cache", QueryOptions: structs.QueryOptions{Region: "global", Namespace: "default", AuthToken: s.RootToken.SecretID},
		}, &srel))
		must.NotNil(t, srel.Release)
		must.True(t, srel.Release.Spec.Pack.ServerRendered, must.Sprint("a built-in module is applied as rendered by the server"))
		must.Eq(t, "builtin/redis", srel.Release.Spec.Pack.Source)
		must.Eq(t, "", srel.Release.Spec.Pack.Registry, must.Sprint("a built-in module is of no registry"))

		// A token that writes releases but can't read registries.
		releasesOnly := mock.CreatePolicyAndToken(t, s.Agent.Server().State(), 1600, "releases-only",
			`namespace "default" { object "rigger.norca.io/Release" { capabilities = ["read", "write"] } }`)
		client.SetSecretID(releasesOnly.SecretID)

		other, _, err := client.Rigger().Install(&api.RiggerInstallRequest{Release: "cache-2", Module: "builtin/redis"}, true, nil)
		must.NoError(t, err)
		must.Eq(t, "install", other.Action)

		_, _, err = client.Rigger().Install(&api.RiggerInstallRequest{Release: "cache-2", Registry: api.RiggerGlobalRegistry, Module: "acme/web"}, true, nil)
		must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	})
}

// rigger install -server, the command of the program against an agent:
// a module of a registry of the cluster by its name (-registry), and a
// module of the global registry, which the servers have not synced, with
// the answer of the servers as they wrote it.
func TestHTTP_RiggerInstallCommand(t *testing.T) {
	// The command reads the cluster from the environment: no parallel test
	// runs beside this one.
	repo := moduleRegistryRepo(t)

	httpACLTest(t, nil, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.RPC)

		client := s.Client()
		client.SetSecretID(s.RootToken.SecretID)

		obj := &api.CustomObject{Group: api.RiggerObjectGroup, Kind: api.RiggerRegistryKind, Name: "acme", Spec: map[string]any{"type": "git", "source": repo}}
		must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
			_, _, err := client.Objects().Apply(obj, nil, nil)

			return err
		}), wait.Timeout(30*time.Second), wait.Gap(100*time.Millisecond)))

		_, _, err := client.Rigger().SyncRegistry("acme", nil)
		must.NoError(t, err)

		// The global registry is there, and not synced.
		must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
			_, _, err := client.Rigger().Install(&api.RiggerInstallRequest{Release: "x", Registry: api.RiggerGlobalRegistry, Module: "acme/web"}, true, nil)
			if err == nil || !strings.Contains(err.Error(), "has not been synced") {
				return fmt.Errorf("the install of a module of the global registry: %v", err)
			}

			return nil
		}), wait.Timeout(30*time.Second), wait.Gap(100*time.Millisecond)))

		t.Setenv("NORCA_ADDR", s.HTTPAddr())
		t.Setenv("NORCA_TOKEN", s.RootToken.SecretID)
		t.Setenv("NORCA_NAMESPACE", "")
		t.Setenv("NORCA_REGION", "")
		t.Setenv("RIGGER_CACHE_DIR", t.TempDir())
		t.Setenv("RIGGER_SERVER", "")

		var stdout, stderr bytes.Buffer

		code := riggercmd.Run(t.Context(), []string{"install", "web", "-registry", "acme", "-name", "web-cli", "-server", "-var", "count=2"}, &stdout, &stderr)
		must.Eq(t, 0, code, must.Sprintf("stderr %s", stderr.String()))
		must.Eq(t, "install default/web-cli: revision 1\n", stdout.String())
		must.StrContains(t, stderr.String(), "is not signed")

		var srel structs.SingleReleaseResponse
		must.NoError(t, s.RPC("Release.GetRelease", &structs.ReleaseSpecificRequest{
			Name: "web-cli", QueryOptions: structs.QueryOptions{Region: "global", Namespace: "default", AuthToken: s.RootToken.SecretID},
		}, &srel))
		must.Eq(t, "acme/web", srel.Release.Spec.Pack.Source)
		must.Eq(t, "acme", srel.Release.Spec.Pack.Registry)
		must.True(t, srel.Release.Spec.Pack.ServerRendered)

		stdout.Reset()
		stderr.Reset()

		code = riggercmd.Run(t.Context(), []string{"install", "acme/web", "-name", "web-global", "-server"}, &stdout, &stderr)
		must.Eq(t, 1, code)
		must.StrContains(t, stderr.String(), `the registry "global" has not been synced: sync it first (rigger registry sync global)`)
	})
}

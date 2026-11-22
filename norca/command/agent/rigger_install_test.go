// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
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

		again, _, err := client.Rigger().Install(req, true, nil)
		must.NoError(t, err)
		must.Eq(t, "unchanged", again.Action)

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

		// A local module sent as files.
		eng, err := rigger.New()
		must.NoError(t, err)

		m, diags := eng.LoadModule(t.Context(), riggerHostModule)
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

		must.NoError(t, s.RPC("Release.GetRelease", &structs.ReleaseSpecificRequest{
			Name: "web-local", QueryOptions: structs.QueryOptions{Region: "global", Namespace: "local", AuthToken: s.RootToken.SecretID},
		}, &srel))
		must.False(t, srel.Release.Spec.Pack.ServerRendered, must.Sprint("a local module is applied as a client's"))

		_, _, err = client.Rigger().Install(&api.RiggerInstallRequest{Release: "x", Registry: "acme", Files: files}, true, nil)
		must.ErrorContains(t, err, "without a registry")

		// A token that writes releases but can't read registries.
		releasesOnly := mock.CreatePolicyAndToken(t, s.Agent.Server().State(), 1600, "releases-only",
			`namespace "default" { object "packs.norca.io/Release" { capabilities = ["read", "write"] } }`)
		client.SetSecretID(releasesOnly.SecretID)

		_, _, err = client.Rigger().Install(&api.RiggerInstallRequest{Release: "web-2", Registry: "acme", Module: "web"}, true, nil)
		must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	})
}

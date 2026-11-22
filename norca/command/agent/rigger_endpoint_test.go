// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/testutil"
)

// A registry of Rigger over the HTTP API, with the api client: written as an
// object, synced, searched, its selection written with a check-and-set; the
// UI's view (the status) changes with the next sync.
func TestHTTP_RiggerRegistry(t *testing.T) {
	ci.Parallel(t)

	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	must.NoError(t, err)

	wt, err := r.Worktree()
	must.NoError(t, err)

	must.NoError(t, os.MkdirAll(filepath.Join(dir, "modules", "web"), 0o750))
	must.NoError(t, os.WriteFile(filepath.Join(dir, "modules", "web", "metadata.hcl"), []byte("module {}\n"), 0o600))
	_, err = wt.Add("modules")
	must.NoError(t, err)

	h, err := wt.Commit("web", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Unix(1700000000, 0)}})
	must.NoError(t, err)

	_, err = r.CreateTag("web/v1.0.0", h, nil)
	must.NoError(t, err)

	httpACLTest(t, nil, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.RPC)

		client := s.Client()
		client.SetSecretID(s.RootToken.SecretID)

		obj := &api.CustomObject{
			Group: api.RiggerObjectGroup, Kind: api.RiggerRegistryKind, Name: "acme",
			Spec: map[string]any{"type": "git", "source": dir, "display": "selected"},
		}

		must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
			_, _, err := client.Objects().Apply(obj, nil, nil)

			return err
		}), wait.Timeout(30*time.Second), wait.Gap(100*time.Millisecond)), must.Sprint("the kind of registries is registered"))

		_, _, err := client.Rigger().SearchRegistry("acme", nil, nil)
		must.ErrorContains(t, err, "has not been synced")

		reg, _, err := client.Rigger().SyncRegistry("acme", nil)
		must.NoError(t, err)
		must.Eq(t, "Ready", reg.Status.Phase)
		must.Eq(t, 1, reg.Status.Modules)
		must.Eq(t, h.String(), reg.Status.Commit)

		found, _, err := client.Rigger().SearchRegistry("acme", &api.RiggerRegistryQuery{Kind: "module", Text: "web"}, nil)
		must.NoError(t, err)
		must.Eq(t, 1, found.Total)
		must.Eq(t, []api.RiggerRegistryItem{{RiggerRegistryRef: api.RiggerRegistryRef{Kind: "module", Name: "web"}, Versions: []string{"1.0.0"}}}, found.Items)

		// Select the module: the status shows it after the next sync.
		reg, _, err = client.Rigger().Registry("acme", nil)
		must.NoError(t, err)

		web := api.RiggerRegistryRef{Kind: "module", Name: "web"}
		_, err = client.Rigger().SetSelected(reg, []api.RiggerRegistryRef{web}, nil)
		must.NoError(t, err)

		// A selection written from an old read is a conflict.
		_, err = client.Rigger().SetSelected(reg, nil, nil)
		must.ErrorIs(t, err, api.ErrObjectConflict)

		reg, _, err = client.Rigger().Registry("acme", nil)
		must.NoError(t, err)
		must.Eq(t, []api.RiggerRegistryRef{web}, reg.Spec.Selected)
		must.SliceEmpty(t, reg.Status.Shown)

		reg, _, err = client.Rigger().SyncRegistry("acme", nil)
		must.NoError(t, err)
		must.Eq(t, []api.RiggerRegistryItem{{RiggerRegistryRef: web, Versions: []string{"1.0.0"}}}, reg.Status.Shown)

		regs, _, err := client.Rigger().Registries(nil)
		must.NoError(t, err)

		names := map[string]bool{}
		for _, reg := range regs {
			names[reg.Name] = true
		}

		must.MapContainsKeys(t, names, []string{"acme", api.RiggerGlobalRegistry})
	})
}

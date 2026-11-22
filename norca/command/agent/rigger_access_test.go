// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/bacanoo-io/norca/rigger/index"
	"github.com/bacanoo-io/norca/rigger/source"
)

// privateRepository is a repository nothing serves: a use of it that is
// allowed fails at the network, one that is not fails before.
const privateRepository = "https://127.0.0.1:1/private"

// privateFetcher stands in for the private repository when a test makes
// the lock file of a stack of it.
type privateFetcher struct{}

func (privateFetcher) Versions(context.Context, source.Request) ([]source.Available, error) {
	return []source.Available{{Version: "1.0.0", Ref: "v1.0.0", Commit: strings.Repeat("a", 40)}}, nil
}

func (privateFetcher) Fetch(_ context.Context, req source.Request) (source.Fetched, error) {
	return source.Fetched{
		FS:     fstest.MapFS{"metadata.hcl": {Data: []byte("module {\n  name    = \"private\"\n  version = \"1.0.0\"\n}\n")}},
		Dir:    ".",
		Origin: source.Origin{Source: req.Source, Version: "1.0.0", Ref: "v1.0.0", Commit: strings.Repeat("a", 40)},
	}, nil
}

// Every use of a registry with an authSecret needs read on its Variable
// beside the rights on the registries: a sync, a search, an install, a
// detail, an icon and a stack, also through another registry of the same
// repository and an entry of the global registry, and the write that would
// take the authSecret away.
func TestHTTP_RiggerRegistryAuth(t *testing.T) {
	ci.Parallel(t)

	idx := newTestRepo(t)

	entry, err := index.Marshal(index.Entry{Namespace: "acme", Name: "private", Versions: []index.Version{{
		Version: "1.0.0", Repository: privateRepository, Path: "modules/private", Tag: "private/v1.0.0",
		Commit: strings.Repeat("a", 40), Digest: "sha256:" + strings.Repeat("b", 64),
		Signatures: []index.Signature{{KeyID: "sha256:" + strings.Repeat("c", 64), Signature: "MEUCIQ=="}}, Listed: "2026-11-22",
	}}})
	must.NoError(t, err)

	idx.commit(map[string]string{index.EntryPath(index.KindModule, "acme", "private"): string(entry)})

	stack := sentStack(t, privateFetcher{}, "priv", privateRepository)

	httpACLTest(t, nil, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.RPC)

		root := s.Client()
		root.SetSecretID(s.RootToken.SecretID)

		// The keyring of a new server is ready a little after its leader.
		must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
			_, _, err := root.Variables().Create(&api.Variable{Path: "creds/private", Items: api.VariableItems{"password": "fixture"}}, nil)

			return err
		}), wait.Timeout(30*time.Second), wait.Gap(100*time.Millisecond)))

		applyTestRegistry(t, root, "private", map[string]any{"type": "git", "source": privateRepository, "authSecret": map[string]any{"path": "creds/private"}})
		applyTestRegistry(t, root, "open", map[string]any{"type": "git", "source": privateRepository})
		applyTestRegistry(t, root, "global", map[string]any{"type": "global", "source": idx.url()})
		syncTestRegistry(t, root, "global")

		store := s.Agent.Server().State()

		// Both had a good sync once.
		for _, name := range []string{"private", "open"} {
			obj, err := store.CustomObjectByName(nil, structs.RiggerObjectGroup, structs.RiggerRegistryKind, "", name)
			must.NoError(t, err)

			synced := obj.Copy()
			synced.Status = map[string]any{"phase": "Ready", "source": privateRepository, "commit": strings.Repeat("a", 40)}

			at, err := store.LatestIndex()
			must.NoError(t, err)
			must.NoError(t, store.UpsertCustomObject(structs.MsgTypeTestSetup, at+1, synced, obj.ModifyIndex))
		}

		const rights = `object "rigger.norca.io/Registry" { capabilities = ["read", "write"] }
  object "packs.norca.io/Release" { capabilities = ["read", "write"] }`

		at, err := store.LatestIndex()
		must.NoError(t, err)

		without := mock.CreatePolicyAndToken(t, store, at+1, "registries-without-variable",
			"namespace \"*\" {\n  "+rights+"\n}\n")
		with := mock.CreatePolicyAndToken(t, store, at+5, "registries-with-variable",
			"namespace \"*\" {\n  "+rights+"\n  variables {\n    path \"creds/*\" {\n      capabilities = [\"read\"]\n    }\n  }\n}\n")

		denied := s.Client()
		denied.SetSecretID(without.SecretID)

		install := func(registry, module string) error {
			_, _, err := denied.Rigger().Install(&api.RiggerInstallRequest{Release: "p", Registry: registry, Module: module}, true, nil)

			return err
		}

		uses := map[string]func() error{
			"sync":   func() error { _, _, err := denied.Rigger().SyncRegistry("private", nil); return err },
			"search": func() error { _, _, err := denied.Rigger().SearchRegistry("private", nil, nil); return err },
			"detail": func() error { _, _, err := denied.Rigger().ModuleDetail("private", "private", "", nil); return err },
			"icon":   func() error { _, err := denied.Rigger().Icon("private", "private", "", nil); return err },
			"stack":  func() error { _, _, err := denied.Rigger().StackPlan(stack, nil); return err },
			"install": func() error {
				return install("private", "private")
			},
			"sync of another registry of the repository": func() error {
				_, _, err := denied.Rigger().SyncRegistry("open", nil)

				return err
			},
			"search of another registry of the repository": func() error {
				_, _, err := denied.Rigger().SearchRegistry("open", nil, nil)

				return err
			},
			"install by an entry of the global registry": func() error {
				return install("global", "acme/private")
			},
		}

		for name, use := range uses {
			err := use()
			must.ErrorContains(t, err, `read on the Variable "creds/private"`, must.Sprint(name))
			must.ErrorContains(t, err, "403", must.Sprint(name))
		}

		obj, err := store.CustomObjectByName(nil, structs.RiggerObjectGroup, structs.RiggerRegistryKind, "", "private")
		must.NoError(t, err)
		must.Eq(t, "Ready", obj.Status["phase"], must.Sprint("a denied sync wrote the status"))

		_, _, err = denied.Objects().Apply(&api.CustomObject{
			Group: api.RiggerObjectGroup, Kind: api.RiggerRegistryKind, Name: "private",
			Spec: map[string]any{"type": "git", "source": privateRepository},
		}, nil, nil)
		must.ErrorContains(t, err, `can't read the Variable "creds/private"`)

		allowed := s.Client()
		allowed.SetSecretID(with.SecretID)

		_, _, err = allowed.Rigger().SyncRegistry("private", nil)
		must.Error(t, err)
		must.StrNotContains(t, err.Error(), "Variable", must.Sprint("with read on the Variable the sync gets to the network"))
	})
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/bacanoo-io/norca/rigger/host"
)

// An overwrite that says what it replaces writes only over that.
func TestRiggerHosts_EnsureReplaces(t *testing.T) {
	ci.Parallel(t)

	s, root, cleanup := TestACLServer(t, nil)
	t.Cleanup(cleanup)
	testutil.WaitForLeader(t, s.RPC)

	secrets := riggerSecrets{&riggerHosts{srv: s, token: root.SecretID, namespace: structs.DefaultNamespace}}
	items := func(v string) map[string]host.Sensitive {
		return map[string]host.Sensitive{"db": host.NewSensitive(v)}
	}

	const path = "rigger/replaces"

	must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
		_, err := secrets.Ensure(t.Context(), host.Secret{Path: path, Items: items("now")})

		return err
	})))

	written, err := secrets.Ensure(t.Context(), host.Secret{Path: path, Items: items("lost"), Overwrite: true, Replaces: items("before")})
	must.NoError(t, err)
	must.False(t, written, must.Sprint("the write of another run was overwritten"))

	stored, err := secrets.Read(t.Context(), path)
	must.NoError(t, err)
	must.Eq(t, "now", stored["db"].Reveal())

	written, err = secrets.Ensure(t.Context(), host.Secret{Path: path, Items: items("next"), Overwrite: true, Replaces: items("now")})
	must.NoError(t, err)
	must.True(t, written)
}

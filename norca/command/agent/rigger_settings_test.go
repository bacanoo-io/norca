// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"testing"
	"testing/fstest"
	"time"

	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/testutil"
	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/cli/norcaapi"
	"github.com/bacanoo-io/norca/rigger/host"
)

// An install by the servers and one by the rigger program are one: it
// waits until the release is ready, for the timeout given, so either finds
// the release of the other unchanged.
func TestHTTP_RiggerInstallSettings(t *testing.T) {
	ci.Parallel(t)

	eng, err := rigger.New()
	must.NoError(t, err)

	m, diags := eng.LoadModule(t.Context(), fstest.MapFS{
		"metadata.hcl": {Data: []byte("module {\n  name    = \"m\"\n  version = \"1.0.0\"\n}\n")},
		"x.var.hcl":    {Data: []byte("variable \"x\" {\n  path = \"settings/one\"\n\n  items {\n    v = \"a\"\n  }\n}\n")},
	})
	must.False(t, diags.HasErrors(), must.Sprint(diags))

	files, err := m.Archive()
	must.NoError(t, err)

	httpACLTest(t, nil, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.RPC)

		client := s.Client()
		client.SetSecretID(s.RootToken.SecretID)

		const timeout = 90 * time.Second

		req := &api.RiggerInstallRequest{Release: "one", Files: files, Timeout: timeout}

		must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
			_, _, err := client.Rigger().Install(req, false, nil)

			return err
		}), wait.Timeout(30*time.Second), wait.Gap(200*time.Millisecond)))

		program, err := rigger.New(rigger.WithReleases(norcaapi.NewReleases(client)))
		must.NoError(t, err)

		rp, diags := program.PlanInstall(t.Context(), m,
			rigger.Install{Release: "one", Namespace: "default", Region: s.Config.Region},
			host.Settings{Wait: true, Timeout: timeout})
		must.False(t, diags.HasErrors(), must.Sprint(diags))
		must.Eq(t, rigger.ActionUnchanged, rp.Action)
	})
}

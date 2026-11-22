// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"testing"
	"testing/fstest"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/host"
	"github.com/shoenig/test/must"
)

// The module of the test: one job and one Variable.
var riggerHostModule = fstest.MapFS{
	"metadata.hcl":  {Data: []byte("module {\n  name    = \"web\"\n  version = \"1.0.0\"\n}\n")},
	"variables.hcl": {Data: []byte("variable \"count\" {\n  type    = number\n  default = 1\n}\n")},
	"web.norca.hcl": {Data: []byte(`job "web" {
  datacenters = ["dc1"]

  group "web" {
    count = var.count

    task "web" {
      driver = "mock_driver"

      config {
        run_for = "1s"
      }
    }
  }
}
`)},
	"web.var.hcl": {Data: []byte("variable \"web\" {\n  path = \"rigger-host/web\"\n\n  items {\n    count = tostring(var.count)\n  }\n}\n")},
}

// An engine of Rigger built from the hosts of the server (design/
// rig-integration.md 4.3) plans and applies a module as the rigger program
// does through the HTTP API: the release is the manager rigger's, its job and
// Variable are written, a second plan is unchanged; the secret store and the
// cluster answer; and a token without the right to write releases is refused.
func TestRiggerHosts_InstallModule(t *testing.T) {
	ci.Parallel(t)

	httpACLTest(t, nil, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.RPC)
		srv := s.Agent.Server()
		ctx := t.Context()

		eng, err := srv.RiggerEngine(s.RootToken.SecretID, structs.DefaultNamespace)
		must.NoError(t, err)

		m, diags := eng.LoadModule(ctx, riggerHostModule)
		must.False(t, diags.HasErrors(), must.Sprint(diags))

		install := rigger.Install{Release: "web", Namespace: structs.DefaultNamespace, Region: srv.Region()}

		rp, diags := eng.PlanInstall(ctx, m, install, host.Settings{})
		must.False(t, diags.HasErrors(), must.Sprint(diags))
		must.Eq(t, rigger.ActionInstall, rp.Action)

		outcome, err := eng.ApplyInstall(ctx, rp, rigger.ApplyOptions{})
		must.NoError(t, err)
		must.Eq(t, uint64(1), outcome.Revision)

		var rel structs.SingleReleaseResponse
		must.NoError(t, s.RPC("Release.GetRelease", &structs.ReleaseSpecificRequest{
			Name: "web", QueryOptions: structs.QueryOptions{Region: srv.Region(), Namespace: structs.DefaultNamespace, AuthToken: s.RootToken.SecretID},
		}, &rel))
		must.NotNil(t, rel.Release)
		must.Eq(t, structs.ReleaseManagerRigger, rel.Release.Spec.Pack.Manager)
		must.Eq(t, "web", rel.Release.Spec.Pack.Name)

		job, err := srv.State().JobByID(nil, structs.DefaultNamespace, "web")
		must.NoError(t, err)
		must.NotNil(t, job)
		must.Eq(t, "web", job.Meta[structs.ReleaseMetaRelease])

		// The same module and values again: unchanged.
		again, diags := eng.PlanInstall(ctx, m, install, host.Settings{})
		must.False(t, diags.HasErrors(), must.Sprint(diags))
		must.Eq(t, rigger.ActionUnchanged, again.Action)

		// A token that may submit jobs but not write releases is refused.
		weak := mock.CreatePolicyAndToken(t, srv.State(), 1500, "jobs-only", `namespace "default" {
  capabilities = ["submit-job", "read-job"]
}`)
		weakEng, err := srv.RiggerEngine(weak.SecretID, structs.DefaultNamespace)
		must.NoError(t, err)

		other := install
		other.Release = "web-other"
		wp, diags := weakEng.PlanInstall(ctx, m, other, host.Settings{})
		if !diags.HasErrors() {
			_, err = weakEng.ApplyInstall(ctx, wp, rigger.ApplyOptions{})
			must.ErrorContains(t, err, "Permission denied")
		} else {
			must.StrContains(t, diags.Error(), "Permission denied")
		}
	})
}

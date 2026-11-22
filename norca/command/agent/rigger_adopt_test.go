// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/testutil"
	rigger "github.com/bacanoo-io/norca/rigger"
)

// A release of the pack manager is taken over by a stack when asked to:
// planned without being asked, refused by an apply without Adopt, applied
// with it.
func TestHTTP_RiggerAdopt_Pack(t *testing.T) {
	ci.Parallel(t)

	eng, err := rigger.New()
	must.NoError(t, err)

	st, diags := eng.LoadStack(t.Context(), fstest.MapFS{
		"s/stack.hcl":              {Data: []byte("rigger {\n  name      = \"shop\"\n  region    = \"global\"\n  namespace = \"default\"\n}\n\nmodule \"taken\" {\n  source = \"./modules/m\"\n}\n")},
		"s/modules/m/metadata.hcl": {Data: []byte("module {\n  name    = \"m\"\n  version = \"1.0.0\"\n}\n")},
		"s/modules/m/x.var.hcl":    {Data: []byte("variable \"x\" {\n  path = \"adopt/taken\"\n\n  items {\n    by = \"rigger\"\n  }\n}\n")},
	}, "s")
	must.False(t, diags.HasErrors(), must.Sprint(diags))

	files, err := st.Archive()
	must.NoError(t, err)

	httpACLTest(t, nil, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.RPC)

		client := s.Client()
		client.SetSecretID(s.RootToken.SecretID)

		byPack := &api.ReleaseApplyRequest{
			Pack: &api.ReleasePack{Name: "taken", Version: "1.0.0"},
			Resources: []*api.ReleaseResource{{
				Type:     "variable",
				Variable: &api.Variable{Namespace: "default", Path: "adopt/taken", Items: api.VariableItems{"by": "pack"}},
			}},
		}

		must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
			_, _, err := client.Releases().Apply("taken", byPack, nil)

			return err
		}), wait.Timeout(30*time.Second), wait.Gap(200*time.Millisecond)))

		req := &api.RiggerStackRequest{Files: files}

		_, _, err := client.Rigger().StackPlan(req, nil)
		must.NoError(t, err, must.Sprint("the plan of a take-over"))

		_, _, err = client.Rigger().StackApply(req, nil)
		must.ErrorContains(t, err, "adopt")
		must.ErrorContains(t, err, "409")

		req.Adopt = true

		op, _, err := client.Rigger().StackApply(req, nil)
		must.NoError(t, err, must.Sprint("the apply with adopt"))

		op = waitOperation(t, client, op.Name)
		must.Eq(t, "Succeeded", op.Status.Phase, must.Sprint(op.Status.Error))

		rel, _, err := client.Releases().Info("taken", nil)
		must.NoError(t, err)
		must.Eq(t, "rigger", rel.Spec.Pack.Manager)

		// One module installed by the servers takes over the same way.
		mod, diags := eng.LoadModule(t.Context(), fstest.MapFS{
			"metadata.hcl": {Data: []byte("module {\n  name    = \"m\"\n  version = \"1.0.0\"\n}\n")},
			"x.var.hcl":    {Data: []byte("variable \"x\" {\n  path = \"adopt/alone\"\n\n  items {\n    by = \"rigger\"\n  }\n}\n")},
		})
		must.False(t, diags.HasErrors(), must.Sprint(diags))

		moduleFiles, err := mod.Archive()
		must.NoError(t, err)

		byPack.Pack.Name = "alone"
		byPack.Resources[0].Variable.Path = "adopt/alone"

		_, _, err = client.Releases().Apply("alone", byPack, nil)
		must.NoError(t, err)

		install := &api.RiggerInstallRequest{Release: "alone", Files: moduleFiles}

		planned, _, err := client.Rigger().Install(install, true, nil)
		must.NoError(t, err, must.Sprint("the plan of an install that takes over"))
		must.StrContains(t, strings.Join(planned.Warnings, "; "), "pack")

		_, _, err = client.Rigger().Install(install, false, nil)
		must.ErrorContains(t, err, "adopt")
		must.ErrorContains(t, err, "409")

		install.Adopt = true

		_, _, err = client.Rigger().Install(install, false, nil)
		must.NoError(t, err, must.Sprint("the install with adopt"))
	})
}

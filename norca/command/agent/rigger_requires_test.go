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
)

// The servers check what a stack and a module require: the version of
// Norca, and a module installed next to them.
func TestHTTP_RiggerRequires(t *testing.T) {
	ci.Parallel(t)

	eng, err := rigger.New()
	must.NoError(t, err)

	archive := func(files fstest.MapFS) []byte {
		m, diags := eng.LoadModule(t.Context(), files)
		must.False(t, diags.HasErrors(), must.Sprint(diags))

		data, err := m.Archive()
		must.NoError(t, err)

		return data
	}

	needs := archive(fstest.MapFS{
		"metadata.hcl": {Data: []byte("module {\n  name    = \"needs\"\n  version = \"1.0.0\"\n\n  requires {\n    module \"rustfs\" {\n      version = \">= 0.2.0\"\n    }\n  }\n}\n")},
		"x.var.hcl":    {Data: []byte("variable \"x\" {\n  path = \"requires/needs\"\n\n  items {\n    v = \"a\"\n  }\n}\n")},
	})
	store := archive(fstest.MapFS{
		"metadata.hcl": {Data: []byte("module {\n  name    = \"rustfs\"\n  version = \"0.2.0\"\n}\n")},
		"x.var.hcl":    {Data: []byte("variable \"x\" {\n  path = \"requires/store\"\n\n  items {\n    v = \"a\"\n  }\n}\n")},
	})

	st, diags := eng.LoadStack(t.Context(), fstest.MapFS{
		"s/stack.hcl":              {Data: []byte("rigger {\n  required_version = \">= 999.0.0\"\n  name             = \"later\"\n  namespace        = \"default\"\n}\n\nmodule \"one\" {\n  source = \"./modules/m\"\n}\n")},
		"s/modules/m/metadata.hcl": {Data: []byte("module {\n  name    = \"m\"\n  version = \"1.0.0\"\n}\n")},
	}, "s")
	must.False(t, diags.HasErrors(), must.Sprint(diags))

	stack, err := st.Archive()
	must.NoError(t, err)

	httpACLTest(t, nil, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.RPC)

		client := s.Client()
		client.SetSecretID(s.RootToken.SecretID)

		_, _, err := client.Rigger().StackPlan(&api.RiggerStackRequest{Files: stack}, nil)
		must.ErrorContains(t, err, "required_version")

		_, _, err = client.Rigger().Install(&api.RiggerInstallRequest{Release: "needs", Files: needs}, false, nil)
		must.ErrorContains(t, err, "rustfs")

		must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
			_, _, err := client.Rigger().Install(&api.RiggerInstallRequest{Release: "s3", Files: store}, false, nil)

			return err
		}), wait.Timeout(30*time.Second), wait.Gap(200*time.Millisecond)))

		_, _, err = client.Rigger().Install(&api.RiggerInstallRequest{Release: "needs", Files: needs}, false, nil)
		must.NoError(t, err)
	})
}

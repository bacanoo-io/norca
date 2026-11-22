// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"testing"
	"testing/fstest"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/testutil"
	rigger "github.com/bacanoo-io/norca/rigger"
)

// A stack is of one region: the servers of another refuse to plan, apply
// and destroy it, where rigger.region would not be true.
func TestHTTP_RiggerStackRegion(t *testing.T) {
	ci.Parallel(t)

	eng, err := rigger.New()
	must.NoError(t, err)

	st, diags := eng.LoadStack(t.Context(), fstest.MapFS{
		"s/stack.hcl":              {Data: []byte("rigger {\n  name      = \"away\"\n  region    = \"elsewhere\"\n  namespace = \"default\"\n}\n\nmodule \"one\" {\n  source = \"./modules/m\"\n}\n")},
		"s/modules/m/metadata.hcl": {Data: []byte("module {\n  name    = \"m\"\n  version = \"1.0.0\"\n}\n")},
		"s/modules/m/x.var.hcl":    {Data: []byte("variable \"x\" {\n  path = \"away/one\"\n\n  items {\n    region = rigger.region\n  }\n}\n")},
	}, "s")
	must.False(t, diags.HasErrors(), must.Sprint(diags))

	files, err := st.Archive()
	must.NoError(t, err)

	httpACLTest(t, nil, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.RPC)

		client := s.Client()
		client.SetSecretID(s.RootToken.SecretID)

		req := &api.RiggerStackRequest{Files: files}
		want := `is of the region "elsewhere"`

		_, _, err := client.Rigger().StackPlan(req, nil)
		must.ErrorContains(t, err, want, must.Sprint("plan"))
		must.ErrorContains(t, err, "400", must.Sprint("plan"))

		_, _, err = client.Rigger().StackApply(req, nil)
		must.ErrorContains(t, err, want, must.Sprint("apply"))

		_, _, err = client.Rigger().StackDestroy(req, nil)
		must.ErrorContains(t, err, want, must.Sprint("destroy"))
	})
}

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

func oneReleaseStack(t *testing.T, name, namespace, release string) *api.RiggerStackRequest {
	t.Helper()

	eng, err := rigger.New()
	must.NoError(t, err)

	st, diags := eng.LoadStack(t.Context(), fstest.MapFS{
		"s/stack.hcl":              {Data: []byte("rigger {\n  name      = \"" + name + "\"\n  region    = \"global\"\n  namespace = \"" + namespace + "\"\n}\n\nmodule \"" + release + "\" {\n  source = \"./modules/m\"\n}\n")},
		"s/modules/m/metadata.hcl": {Data: []byte("module {\n  name    = \"m\"\n  version = \"1.0.0\"\n}\n")},
		"s/modules/m/x.var.hcl":    {Data: []byte("variable \"x\" {\n  path = \"owner/${rigger.release}\"\n\n  items {\n    by = \"rigger\"\n  }\n}\n")},
	}, "s")
	must.False(t, diags.HasErrors(), must.Sprint(diags))

	files, err := st.Archive()
	must.NoError(t, err)

	return &api.RiggerStackRequest{Files: files}
}

// Two stacks of one name in two namespaces on the servers: the destroy of
// one leaves the releases of the other, and a third stack takes a release
// of the first only with adopt.
func TestHTTP_RiggerOwner(t *testing.T) {
	ci.Parallel(t)

	first := oneReleaseStack(t, "shop", "default", "web")
	other := oneReleaseStack(t, "shop", "other", "api")
	taker := oneReleaseStack(t, "mall", "default", "web")

	httpACLTest(t, nil, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.RPC)

		client := s.Client()
		client.SetSecretID(s.RootToken.SecretID)

		_, err := client.Namespaces().Register(&api.Namespace{Name: "other"}, nil)
		must.NoError(t, err)

		inOther := &api.WriteOptions{Namespace: "other"}

		var op *api.RiggerOperation

		// The kinds and the keyring of a new server come a little after its leader.
		must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
			var err error
			op, _, err = client.Rigger().StackApply(first, nil)

			return err
		}), wait.Timeout(30*time.Second), wait.Gap(200*time.Millisecond)))
		must.Eq(t, "Succeeded", waitOperation(t, client, op.Name).Status.Phase)

		plan, _, err := client.Rigger().StackPlan(other, inOther)
		must.NoError(t, err)
		must.Len(t, 1, plan.Plan.Releases, must.Sprint("other/shop removes nothing of default/shop"))

		destroy, _, err := client.Rigger().StackDestroy(other, inOther)
		must.NoError(t, err)

		scoped := s.Client()
		scoped.SetSecretID(s.RootToken.SecretID)
		scoped.SetNamespace("other")
		must.Eq(t, "Succeeded", waitOperation(t, scoped, destroy.Name).Status.Phase)

		_, _, err = client.Releases().Info("web", nil)
		must.NoError(t, err, must.Sprint("the destroy of other/shop removed the release of default/shop"))

		_, _, err = client.Rigger().StackApply(taker, nil)
		must.ErrorContains(t, err, "default/shop")
		must.ErrorContains(t, err, "409")
	})
}

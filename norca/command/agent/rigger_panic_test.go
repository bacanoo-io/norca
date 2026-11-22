// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	rigger "github.com/bacanoo-io/norca/rigger"
)

// A panic under an operation of Rigger is an error of that operation: the
// stack is free for the next one (a plan that panicked), and an operation
// that panicked while it ran ends Failed instead of ending the server.
func TestHTTP_RiggerStackPanic(t *testing.T) {
	ci.Parallel(t)

	eng, err := rigger.New()
	must.NoError(t, err)

	archive := func(fsys fstest.MapFS) []byte {
		st, diags := eng.LoadStack(t.Context(), fsys, "s")
		must.False(t, diags.HasErrors(), must.Sprint(diags))

		files, err := st.Archive()
		must.NoError(t, err)

		return files
	}

	shop := archive(stackOpFiles)
	solo := archive(fstest.MapFS{
		"s/stack.hcl":              {Data: []byte("rigger {\n  name      = \"solo\"\n  namespace = \"default\"\n}\n\nmodule \"one\" {\n  source = \"./modules/m\"\n}\n")},
		"s/modules/m/metadata.hcl": {Data: []byte("module {\n  name    = \"m\"\n  version = \"1.0.0\"\n}\n")},
		"s/modules/m/one.var.hcl":  {Data: []byte("variable \"one\" {\n  path = \"solo/one\"\n}\n")},
	})

	httpACLTest(t, nil, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.RPC)

		// The conversion of a release request is the host the servers are
		// given; it panics while fault is set.
		var fault atomic.Bool

		cfg := s.Agent.Server().GetConfig()
		convert := cfg.ReleaseFromRendered
		cfg.ReleaseFromRendered = func(name string, req *api.ReleaseApplyRequest, ns string) (*structs.ReleaseApplyRequest, error) {
			if fault.Load() {
				panic("a fault under the engine")
			}

			return convert(name, req, ns)
		}

		client := s.Client()
		client.SetSecretID(s.RootToken.SecretID)

		var first *api.RiggerOperation

		must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
			var err error
			first, _, err = client.Rigger().StackApply(&api.RiggerStackRequest{Files: shop}, nil)

			return err
		}), wait.Timeout(30*time.Second), wait.Gap(100*time.Millisecond)))
		must.Eq(t, "Succeeded", waitOperation(t, client, first.Name).Status.Phase)

		// The plan of an upgrade asks the host, and panics.
		bigger := &api.RiggerStackRequest{Files: shop, Values: "size = 2\n"}

		fault.Store(true)

		_, _, err := client.Rigger().StackApply(bigger, nil)
		must.Error(t, err)

		fault.Store(false)

		again, _, err := client.Rigger().StackApply(bigger, nil)
		must.NoError(t, err, must.Sprint("the stack is free after the panic of its plan"))
		must.Eq(t, "Succeeded", waitOperation(t, client, again.Name).Status.Phase)

		// The apply of an install asks the host in the goroutine of the
		// operation.
		fault.Store(true)

		failed, _, err := client.Rigger().StackApply(&api.RiggerStackRequest{Files: solo}, nil)
		must.NoError(t, err)

		op := waitOperation(t, client, failed.Name)
		must.Eq(t, "Failed", op.Status.Phase)
		must.StrContains(t, op.Status.Error, "internal error")
	})
}

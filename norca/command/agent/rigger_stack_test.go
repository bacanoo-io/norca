// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/testutil"
	rigger "github.com/bacanoo-io/norca/rigger"
)

// stackOpFiles is a local stack of two releases of Variables: db, whose
// password the apply generates, and app, which reads the output of db.
var stackOpFiles = fstest.MapFS{
	"s/stack.hcl": {Data: []byte(`rigger {
  name      = "shop"
  region    = "global"
  namespace = "default"
}

module "app" {
  source = "./modules/app"
  values = { db = module.db.service }
}

module "db" {
  source = "./modules/db"
  values = { size = var.size, password = "!generate:db" }
}
`)},
	"s/variables.hcl":             {Data: []byte("variable \"size\" {\n  default = 1\n}\n")},
	"s/modules/db/metadata.hcl":   {Data: []byte("module {\n  name    = \"db\"\n  version = \"1.0.0\"\n}\n")},
	"s/modules/db/variables.hcl":  {Data: []byte("variable \"size\" {}\n\nvariable \"password\" {\n  sensitive = true\n}\n")},
	"s/modules/db/db.var.hcl":     {Data: []byte("variable \"db\" {\n  path = \"shop/db\"\n\n  items {\n    size     = var.size\n    password = var.password\n  }\n}\n")},
	"s/modules/db/outputs.hcl":    {Data: []byte("output \"service\" {\n  value = \"${rigger.release}-svc\"\n}\n")},
	"s/modules/app/metadata.hcl":  {Data: []byte("module {\n  name    = \"app\"\n  version = \"1.0.0\"\n}\n")},
	"s/modules/app/variables.hcl": {Data: []byte("variable \"db\" {}\n")},
	"s/modules/app/app.var.hcl":   {Data: []byte("variable \"app\" {\n  path = \"shop/app\"\n\n  items {\n    db = var.db\n  }\n}\n")},
}

// waitOperation waits until an operation has ended and returns it.
func waitOperation(t *testing.T, client *api.Client, name string) *api.RiggerOperation {
	t.Helper()

	var op *api.RiggerOperation

	must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
		got, _, err := client.Rigger().Operation(name, nil)
		if err != nil {
			return err
		}

		if got.Status.Phase == "Running" {
			return fmt.Errorf("%s is running", name)
		}

		op = got

		return nil
	}), wait.Timeout(60*time.Second), wait.Gap(100*time.Millisecond)))

	return op
}

// A local stack sent as files, on the server (design/rig-integration.md
// 4.6b): planned; applied as an operation that ends Succeeded with its
// outcomes and its full log on the bus; applied again (unchanged); its
// status; a cancel of an ended operation is refused; the operations beyond
// ten are deleted; destroyed as an operation; a token without the right to
// write releases is refused.
func TestHTTP_RiggerStackOperations(t *testing.T) {
	ci.Parallel(t)

	eng, err := rigger.New()
	must.NoError(t, err)

	st, diags := eng.LoadStack(t.Context(), stackOpFiles, "s")
	must.False(t, diags.HasErrors(), must.Sprint(diags))

	files, err := st.Archive()
	must.NoError(t, err)

	// The bus of the server, for the logs of the operations.
	withBus := func(c *Config) { c.Server.Bus = &BusConfig{Enabled: pointer.Of(true)} }

	httpACLTest(t, withBus, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.RPC)

		client := s.Client()
		client.SetSecretID(s.RootToken.SecretID)

		req := &api.RiggerStackRequest{Files: files}

		var plan *api.RiggerStackPlanResponse

		// The kinds of Rigger are registered by the leader after its
		// election.
		must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
			var err error
			plan, _, err = client.Rigger().StackPlan(req, nil)

			return err
		}), wait.Timeout(30*time.Second), wait.Gap(100*time.Millisecond)))

		must.Eq(t, "shop", plan.Plan.Stack)
		must.Eq(t, "db:install app:install", stackActions(plan.Plan.Releases))

		started, _, err := client.Rigger().StackApply(req, nil)
		must.NoError(t, err)
		must.Eq(t, "shop", started.Spec.Stack)
		must.Eq(t, "apply", started.Spec.Action)
		must.StrHasPrefix(t, "sha256:", started.Spec.FilesDigest)

		op := waitOperation(t, client, started.Name)
		must.Eq(t, "Succeeded", op.Status.Phase, must.Sprint(op.Status.Error))
		must.Eq(t, 2, len(op.Status.Outcomes))
		must.Eq(t, uint64(1), op.Status.Outcomes[0].Revision)
		must.True(t, op.Status.LogStored, must.Sprintf("%+v", op.Status))

		log, _, err := client.Rigger().OperationLog(started.Name, 0, 0, 0, nil)
		must.NoError(t, err)
		must.True(t, log.Finished)
		must.Positive(t, len(log.Lines))
		must.StrContains(t, fmt.Sprint(log.Lines), "stack.apply")
		must.StrContains(t, fmt.Sprint(log.Lines), "DEBUG", must.Sprint("the full log has every level"))

		// A page after the first record.
		rest, _, err := client.Rigger().OperationLog(started.Name, log.Lines[0].Seq, 2, 0, nil)
		must.NoError(t, err)
		must.Eq(t, 2, len(rest.Lines))
		must.Eq(t, log.Lines[1].Seq, rest.Lines[0].Seq)
		must.False(t, rest.Finished, must.Sprint("a full page may have more after it"))

		_, _, err = client.Rigger().CancelOperation(started.Name, nil)
		must.ErrorContains(t, err, "has ended")

		status, _, err := client.Rigger().StackStatus(req, nil)
		must.NoError(t, err)
		must.Eq(t, 2, len(status.Releases))

		// Eleven more applies, all unchanged: ten operations are kept.
		var last string

		for range 11 {
			again, _, err := client.Rigger().StackApply(req, nil)
			must.NoError(t, err)

			op = waitOperation(t, client, again.Name)
			must.Eq(t, "Succeeded", op.Status.Phase, must.Sprint(op.Status.Error))
			must.Eq(t, "unchanged", op.Status.Plan[0].Action)

			last = again.Name
		}

		must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
			objs, _, err := client.Objects().List(api.RiggerObjectGroup, api.RiggerOperationKind, nil, nil)
			if err != nil {
				return err
			}

			if len(objs) != 10 {
				return fmt.Errorf("%d operations", len(objs))
			}

			return nil
		}), wait.Timeout(30*time.Second), wait.Gap(200*time.Millisecond)), must.Sprint("ten operations of the stack are kept"))

		_, _, err = client.Rigger().Operation(started.Name, nil)
		must.ErrorContains(t, err, "404", must.Sprint("the oldest operation is deleted"))

		_, _, err = client.Rigger().Operation(last, nil)
		must.NoError(t, err)

		destroy, _, err := client.Rigger().StackDestroy(req, nil)
		must.NoError(t, err)

		op = waitOperation(t, client, destroy.Name)
		must.Eq(t, "Succeeded", op.Status.Phase, must.Sprint(op.Status.Error))
		must.Eq(t, "app:remove db:remove", operationActions(op.Status.Outcomes))

		// A token that can't write releases.
		weak := mock.CreatePolicyAndToken(t, s.Agent.Server().State(), 1700, "read-only",
			`namespace "default" { object "packs.norca.io/Release" { capabilities = ["read"] } }`)
		client.SetSecretID(weak.SecretID)

		_, _, err = client.Rigger().StackPlan(req, nil)
		must.ErrorContains(t, err, "Permission denied")
	})
}

func stackActions(rps []api.RiggerReleasePlan) string {
	parts := make([]string, 0, len(rps))
	for _, rp := range rps {
		parts = append(parts, rp.Release.Name+":"+rp.Action)
	}

	return strings.Join(parts, " ")
}

func operationActions(rs []api.RiggerOperationRelease) string {
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		parts = append(parts, r.Release+":"+r.Action)
	}

	return strings.Join(parts, " ")
}

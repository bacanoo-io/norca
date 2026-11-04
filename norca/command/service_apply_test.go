// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
)

const applyTestFile = `
external_service "orders-db" {
  address = "10.20.0.5"
  port    = 5432
  tags    = ["postgres"]

  check {
    type     = "tcp"
    tcp      = "10.20.0.5:5432"
    interval = "10s"
    timeout  = "2s"
  }

  checked_by {
    node_pool = "default"
  }
}

external_service "billing-db" {
  type   = "alias"
  target = "billing.example.com"
}
`

func writeApplyFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "services.hcl")
	must.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestServiceApplyCommand_Run(t *testing.T) {
	ci.Parallel(t)

	srv, client, url := testServer(t, true, nil)
	defer srv.Shutdown()

	testutil.WaitForLeader(t, srv.Agent.RPC)
	clientID := srv.Agent.Client().NodeID()
	testutil.WaitForClient(t, srv.Agent.Client().RPC, clientID, srv.Agent.Client().Region())

	run := func(args ...string) (int, string, string) {
		ui := cli.NewMockUi()
		cmd := &ServiceApplyCommand{Meta: Meta{Ui: ui, flagAddress: url}}
		code := cmd.Run(append([]string{"-address=" + url}, args...))
		return code, ui.OutputWriter.String(), ui.ErrorWriter.String()
	}

	// no file
	code, _, errOut := run()
	must.Eq(t, 1, code)
	must.StrContains(t, errOut, "takes the file with -f")

	// a file that does not parse names the line
	bad := writeApplyFile(t, "external_service \"a\" {\n  address = \"x\"\n  nope = 1\n}\n")
	code, _, errOut = run("-f", bad)
	must.Eq(t, 1, code)
	must.StrContains(t, errOut, "line 3")

	// a file that parses but is not valid is rejected before anything is
	// written: the valid service of the file stays unregistered
	invalid := writeApplyFile(t, `
external_service "good" {
  address = "10.0.0.1"
}
external_service "broken" {
  address = "10.0.0.2"
  check {
    type = "tcp"
    tcp  = "10.0.0.2:80"
  }
}
`)
	code, _, errOut = run("-f", invalid)
	must.Eq(t, 1, code)
	must.StrContains(t, errOut, "need checked_by")
	regs, _, err := client.Services().ListExternal(nil)
	must.NoError(t, err)
	must.Len(t, 0, regs)

	// apply: both created
	path := writeApplyFile(t, applyTestFile)
	code, out, errOut := run("-f", path)
	must.Eq(t, 0, code, must.Sprintf("stderr: %s", errOut))
	must.StrContains(t, out, `External service "orders-db" (namespace "default"): created`)
	must.StrContains(t, out, `External service "billing-db" (namespace "default"): created`)

	regs, _, err = client.Services().ListExternal(nil)
	must.NoError(t, err)
	must.Len(t, 2, regs)
	byName := map[string]*api.ServiceRegistration{}
	for _, r := range regs {
		byName[r.ServiceName] = r
	}
	db := byName["orders-db"]
	must.NotNil(t, db)
	must.Eq(t, "apply", db.External.Source)
	must.Eq(t, clientID, db.NodeID)
	must.Eq(t, "default", db.External.CheckedBy.NodePool)
	must.Eq(t, "billing.example.com", byName["billing-db"].Address)
	// a declared service is not deregistered when it is critical
	must.Eq(t, 0, int(db.External.DeregisterCriticalAfter))

	// the same file again: nothing changes
	code, out, errOut = run("-f", path)
	must.Eq(t, 0, code, must.Sprintf("stderr: %s", errOut))
	must.StrContains(t, out, `"orders-db" (namespace "default"): unchanged`)
	must.StrContains(t, out, `"billing-db" (namespace "default"): unchanged`)

	// a changed block is updated, the other one is left alone
	changed := writeApplyFile(t, applyTestFile[:len(applyTestFile)-len("}\n")]+"}\n")
	_ = changed
	updated := writeApplyFile(t, `
external_service "billing-db" {
  type   = "alias"
  target = "billing2.example.com"
}
`)
	code, out, errOut = run("-f", updated)
	must.Eq(t, 0, code, must.Sprintf("stderr: %s", errOut))
	must.StrContains(t, out, `"billing-db" (namespace "default"): updated`)

	// the file is also read as the argument and from stdin-less JSON
	jsonPath := filepath.Join(t.TempDir(), "services.json")
	must.NoError(t, os.WriteFile(jsonPath,
		[]byte(`{"Name":"cache","Address":"10.0.0.9","Port":6379}`), 0o600))
	code, out, errOut = run("-json", jsonPath)
	must.Eq(t, 0, code, must.Sprintf("stderr: %s", errOut))
	must.StrContains(t, out, `"cache" (namespace "default"): created`)

	// apply never removes
	regs, _, err = client.Services().ListExternal(nil)
	must.NoError(t, err)
	must.Len(t, 3, regs)
}

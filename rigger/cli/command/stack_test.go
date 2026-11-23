// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"strings"
	"testing"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/host"
)

// A -var of a stack is the value of its variable by the type of the
// variable, as a -var of a module is.
func TestRun_StackValidate_VarByType(t *testing.T) {
	t.Setenv("RIGGER_CACHE_DIR", t.TempDir())

	dir := writeTree(t, map[string]string{
		stackFile: "rigger {\n  name = \"typed\"\n}\n",
		variablesFile: "variable \"tag\" {\n  type = string\n\n  validation {\n    pattern = \"^1[.]20$\"\n  }\n}\n\n" +
			"variable \"replicas\" {\n  type = number\n\n  validation {\n    min = 1000\n    max = 1000\n  }\n}\n",
	})

	riggerRun(t, exitOK, []string{isValid}, "stack", "validate", dir, flagVar, "tag=1.20", flagVar, "replicas=1e3")
	riggerRunErr(t, "var.tag", "stack", "validate", dir, flagVar, "tag=1.2", flagVar, "replicas=1e3")
}

// The plan of a disabled release the stack installed before says that it
// stays installed, and at which revision; one never installed is only
// disabled.
func TestWritePlan_DisabledInstalled(t *testing.T) {
	t.Parallel()

	plan := &rigger.StackPlan{Stack: "site", Region: "eu", Releases: []rigger.ReleasePlan{
		{
			Release: host.ReleaseRef{Namespace: "apps", Name: "app"}, Action: rigger.ActionDisabled, Revision: 3,
			Module: host.ModuleRef{Name: "app", Version: "1.0.0"},
		},
		{Release: host.ReleaseRef{Namespace: "apps", Name: "worker"}, Action: rigger.ActionDisabled},
	}}

	var out bytes.Buffer
	if err := writePlan(&out, plan, false, true); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(out.String(), "\n")
	if len(lines) < 3 {
		t.Fatalf("the plan: %s", out.String())
	}

	if !strings.Contains(lines[1], "app") || !strings.Contains(lines[1], "disabled (stays installed, revision 3)") || !strings.Contains(lines[1], "app 1.0.0") {
		t.Errorf("the line of app = %q, want disabled (stays installed, revision 3) and its module", lines[1])
	}

	if !strings.Contains(lines[2], "worker") || strings.Contains(lines[2], "stays installed") {
		t.Errorf("the line of worker = %q, want disabled and no more", lines[2])
	}
}

// The variables of a stack take RIGGER_VAR_<name> as those of a module do.
func TestRun_StackValidate_EnvVar(t *testing.T) {
	dir := writeTree(t, map[string]string{
		stackFile:     "rigger {\n  name = \"typed\"\n}\n",
		variablesFile: "variable \"tag\" {\n  type = string\n\n  validation {\n    pattern = \"^1[.]20$\"\n  }\n}\n",
	})

	riggerRunErr(t, `The variable "tag" has no default`, "stack", "validate", dir)

	t.Setenv(envTag, "1.20")
	riggerRun(t, exitOK, []string{isValid}, "stack", "validate", dir)
	riggerRunErr(t, "var.tag", "stack", "validate", dir, flagVar, "tag=1.2")
}

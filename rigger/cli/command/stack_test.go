// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"testing"
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

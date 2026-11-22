// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"strings"
	"testing"
)

// The writer of HCL shows the values an expression reads beside its error:
// never a sensitive one, which it can't read without a panic either.
func TestRun_SensitiveInDiagnostics(t *testing.T) {
	t.Parallel()

	const hidden = "do-not-show-me"

	dir := writeTree(t, map[string]string{
		metadataFile:    "module {\n  name    = \"m\"\n  version = \"1.0.0\"\n}\n",
		"variables.hcl": "variable \"v\" {\n  sensitive = true\n}\n",
		"x.var.hcl":     "variable \"x\" {\n  path = \"p\"\n\n  items {\n    a = var.v + 1\n  }\n}\n",
	})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic: %v", r)
		}
	}()

	var stdout, stderr bytes.Buffer

	if code := Run(t.Context(), []string{cmdModule, cmdRender, dir, "-var", "v=" + hidden}, &stdout, &stderr); code != exitError {
		t.Fatalf("exit = %d, want %d: %s", code, exitError, stderr.String())
	}

	if out := stdout.String() + stderr.String(); strings.Contains(out, hidden) {
		t.Errorf("the output shows the sensitive value:\n%s", out)
	}
}

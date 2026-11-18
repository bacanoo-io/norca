// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stackFile is the file of a stack the tests write.
const stackFile = "stack.hcl"

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()

	for name, data := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

// rigRun runs the rig program and checks its exit code and output.
func rigRun(t *testing.T, code int, wants []string, args ...string) string {
	t.Helper()

	var stdout, stderr bytes.Buffer

	got := Run(t.Context(), append([]string{flagLogLevel, "warn"}, args...), &stdout, &stderr)
	if got != code {
		t.Fatalf("rig %s = %d, want %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), got, code, stdout.String(), stderr.String())
	}

	for _, want := range wants {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("rig %s: stdout does not contain %q:\n%s", strings.Join(args, " "), want, stdout.String())
		}
	}

	return stdout.String()
}

// The test is not parallel: it owns the releases and the stack "rigit" of
// the agent.

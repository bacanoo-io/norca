// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun_ModuleValidate(t *testing.T) {
	t.Parallel()

	broken := t.TempDir()
	files := map[string]string{
		metadataFile:            "module {\n  name    = \"app\"\n  version = \"1.0.0\"\n}\n",
		variablesFile:           "variable \"mode\" {\n  default = \"a\"\n\n  validation {\n    one_of = [\"a\", \"b\"]\n  }\n}\n",
		"examples/bad.vars.hcl": "mode = \"c\"\n",
	}

	for name, data := range files {
		p := filepath.Join(broken, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	missing := filepath.Join(broken, "missing")

	// The system words the error, and each system in its own way.
	var notFound *fs.PathError
	if _, err := os.Stat(missing); !errors.As(err, &notFound) {
		t.Fatalf("Stat(%s) = %v", missing, err)
	}

	tests := []struct {
		name           string
		args           []string
		expectedCode   int
		expectedStdout string
		expectedStderr string
	}{
		{
			name:           "a valid module",
			args:           []string{cmdModule, cmdValidate, "../../examples/registry/modules/redis"},
			expectedCode:   exitOK,
			expectedStdout: "module redis 0.4.0 in ../../examples/registry/modules/redis is valid",
		},
		{
			name:           "an invalid example",
			args:           []string{cmdModule, cmdValidate, broken},
			expectedCode:   exitError,
			expectedStderr: `Error: Invalid value on examples/bad.vars.hcl line 1: 1: mode = "c" var.mode is "c", not one of "a", "b".`,
		},
		{
			name:           "no directory",
			args:           []string{cmdModule, cmdValidate, missing},
			expectedCode:   exitError,
			expectedStderr: notFound.Err.Error(),
		},
		{name: "no module command", args: []string{cmdModule}, expectedCode: exitError, expectedStderr: "module needs a command"},
		{name: "unknown module command", args: []string{cmdModule, "share"}, expectedCode: exitError, expectedStderr: `unknown module command "share"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer

			code := Run(t.Context(), tt.args, &stdout, &stderr)
			if code != tt.expectedCode {
				t.Errorf("Run() = %d, want %d (stderr %q)", code, tt.expectedCode, stderr.String())
			}

			if !strings.Contains(stdout.String(), tt.expectedStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.expectedStdout)
			}

			if !strings.Contains(strings.Join(strings.Fields(stderr.String()), " "), tt.expectedStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.expectedStderr)
			}
		})
	}
}

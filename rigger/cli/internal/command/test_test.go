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

func TestRun_ModuleTest(t *testing.T) {
	t.Parallel()

	failing := t.TempDir()
	files := map[string]string{
		metadataFile:          "module {\n  name    = \"app\"\n  version = \"1.0.0\"\n}\n",
		"x.acl.hcl":           "policy \"x\" {\n  rules = \"r\"\n}\n",
		"tests/x.rigtest.hcl": "test \"holds\" {\n  assert {\n    condition     = resource.policy.x.rules == \"r\"\n    error_message = \"r\"\n  }\n}\n\ntest \"fails\" {\n  assert {\n    condition     = resource.policy.x.rules == \"s\"\n    error_message = \"the rules are s\"\n  }\n}\n",
	}

	for name, data := range files {
		p := filepath.Join(failing, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name           string
		args           []string
		expectedCode   int
		expectedStdout []string
	}{
		{
			name:           "the tests of nats",
			args:           []string{cmdModule, cmdTest, "../../../examples/registry/modules/nats"},
			expectedCode:   exitOK,
			expectedStdout: []string{"ok   tests/nats.rigtest.hcl: three servers form a cluster over the datacenters", "6 passed, 0 failed"},
		},
		{
			name:         "a failing test",
			args:         []string{cmdModule, cmdTest, failing},
			expectedCode: exitError,
			expectedStdout: []string{
				"ok   tests/x.rigtest.hcl: holds", "FAIL tests/x.rigtest.hcl: fails",
				"tests/x.rigtest.hcl:10,21-51: the rules are s", "1 passed, 1 failed",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer

			code := Run(t.Context(), tt.args, &stdout, &stderr)
			if code != tt.expectedCode {
				t.Errorf("Run() = %d, want %d (stderr %q)", code, tt.expectedCode, stderr.String())
			}

			for _, want := range tt.expectedStdout {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout = %s, want it to contain %q", stdout.String(), want)
				}
			}
		})
	}
}

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

// The files of the tests.
const (
	varsFile      = "m/variables.hcl"
	variablesFile = "variables.hcl"
	flagLogLevel  = "-log-level"
)

func TestRun_Fmt(t *testing.T) {
	t.Parallel()

	const (
		unformatted = "variable \"a\" {\ndefault=1\n  description = \"x\"\n}\n"
		formatted   = "variable \"a\" {\n  default     = 1\n  description = \"x\"\n}\n"
	)

	tests := []struct {
		name           string
		files          map[string]string
		check          bool
		expectedCode   int
		expectedStdout string
		expectedStderr string
		expectedFiles  map[string]string
	}{
		{
			name:           "formats",
			files:          map[string]string{varsFile: unformatted, "m/ok.hcl": formatted, "m/README.md": unformatted},
			expectedCode:   exitOK,
			expectedStdout: variablesFile,
			expectedFiles:  map[string]string{varsFile: formatted, "m/README.md": unformatted},
		},
		{
			name:           "checks",
			files:          map[string]string{varsFile: unformatted},
			check:          true,
			expectedCode:   exitError,
			expectedStdout: variablesFile,
			expectedStderr: "files are not formatted",
			expectedFiles:  map[string]string{varsFile: unformatted},
		},
		{
			name:         "hidden directories are left out",
			files:        map[string]string{"m/.rigger/state.hcl": unformatted},
			check:        true,
			expectedCode: exitOK,
		},
		{
			name:           "a file that does not parse",
			files:          map[string]string{"m/broken.hcl": "variable {\n"},
			expectedCode:   exitError,
			expectedStderr: "Error: ",
			expectedFiles:  map[string]string{"m/broken.hcl": "variable {\n"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			for name, data := range tt.files {
				p := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
					t.Fatal(err)
				}

				if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			args := []string{cmdFmt}
			if tt.check {
				args = append(args, "-check")
			}

			var stdout, stderr bytes.Buffer

			code := Run(t.Context(), append(args, filepath.Join(dir, "m")), &stdout, &stderr)
			if code != tt.expectedCode {
				t.Errorf("Run() = %d, want %d (stderr %q)", code, tt.expectedCode, stderr.String())
			}

			if !strings.Contains(stdout.String(), tt.expectedStdout) || !strings.Contains(stderr.String(), tt.expectedStderr) {
				t.Errorf("stdout, stderr = %q, %q, want %q, %q", stdout.String(), stderr.String(), tt.expectedStdout, tt.expectedStderr)
			}

			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}

			defer root.Close() //nolint:errcheck // read only

			for name, want := range tt.expectedFiles {
				got, err := root.ReadFile(name)
				if err != nil {
					t.Fatal(err)
				}

				if string(got) != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
		})
	}
}

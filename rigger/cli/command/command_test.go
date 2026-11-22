// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		args           []string
		expectedCode   int
		expectedStdout string
		expectedStderr string
	}{
		{name: "version", args: []string{cmdVersion}, expectedCode: exitOK, expectedStdout: "rigger "},
		{name: "help command", args: []string{cmdHelp}, expectedCode: exitOK, expectedStdout: "Usage: rigger"},
		{name: "help flag", args: []string{"-h"}, expectedCode: exitOK, expectedStderr: "Usage: rigger"},
		{name: "no command", args: nil, expectedCode: exitError, expectedStderr: "Usage: rigger"},
		{name: "unknown command", args: []string{"nope"}, expectedCode: exitError, expectedStderr: `rigger: unknown command "nope"`},
		{name: "unknown flag", args: []string{"-nope", cmdVersion}, expectedCode: exitError, expectedStderr: "flag provided but not defined"},
		{
			name:           "invalid log format",
			args:           []string{"-log-format", "xml", cmdVersion},
			expectedCode:   exitError,
			expectedStderr: `rigger: invalid log format "xml"`,
		},
		{
			name:           "invalid log level",
			args:           []string{flagLogLevel, "loud", cmdVersion},
			expectedCode:   exitError,
			expectedStderr: `rigger: invalid log level "loud"`,
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

			if !strings.Contains(stdout.String(), tt.expectedStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.expectedStdout)
			}

			if !strings.Contains(stderr.String(), tt.expectedStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.expectedStderr)
			}
		})
	}
}

// norca runs the commands as "norca rigger": the usage names it, and the
// version is the one norca gives.
func TestRunProgram(t *testing.T) {
	t.Parallel()

	prog := Program{Name: "norca rigger", Version: "(norca 1.2.3)"}

	tests := []struct {
		name           string
		args           []string
		expectedCode   int
		expectedStdout string
	}{
		{name: "version", args: []string{cmdVersion}, expectedCode: exitOK, expectedStdout: "rigger (norca 1.2.3)\n"},
		{name: "help", args: []string{cmdHelp}, expectedCode: exitOK, expectedStdout: "Usage: norca rigger [flags] <command>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer

			code := RunProgram(t.Context(), prog, tt.args, &stdout, &stderr)
			if code != tt.expectedCode {
				t.Errorf("RunProgram() = %d, want %d (stderr %q)", code, tt.expectedCode, stderr.String())
			}

			if !strings.Contains(stdout.String(), tt.expectedStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.expectedStdout)
			}
		})
	}
}

func TestRun_Log(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		format   string
		expected []string
	}{
		{
			name:   "json",
			format: "json",
			expected: []string{
				`"msg":"started","op":"command.version"`,
				`"msg":"finished","op":"command.version"`,
			},
		},
		{
			name:     "tree",
			format:   "text",
			expected: []string{"DEBUG command.version started", "DEBUG command.version finished duration="},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer

			code := Run(t.Context(), []string{"-log-format", tt.format, flagLogLevel, "debug", cmdVersion}, &stdout, &stderr)
			if code != exitOK {
				t.Fatalf("Run() = %d, want %d (stderr %q)", code, exitOK, stderr.String())
			}

			lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
			if len(lines) != len(tt.expected) {
				t.Fatalf("got %d log lines, want %d:\n%s", len(lines), len(tt.expected), stderr.String())
			}

			for i, want := range tt.expected {
				if !strings.Contains(lines[i], want) {
					t.Errorf("line %d = %q, want it to contain %q", i, lines[i], want)
				}
			}
		})
	}
}

func TestRun_JSONLogHasTheIDs(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer

	Run(t.Context(), []string{"-log-format", "json", flagLogLevel, "debug", cmdVersion}, &stdout, &stderr)

	var record map[string]any
	if err := json.Unmarshal([]byte(strings.Split(stderr.String(), "\n")[0]), &record); err != nil {
		t.Fatalf("the first log line is not JSON: %v", err)
	}

	for _, key := range []string{"trace_id", "span_id", "depth"} {
		if _, ok := record[key]; !ok {
			t.Errorf("the line %v has no %s", record, key)
		}
	}
}

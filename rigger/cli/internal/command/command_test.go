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
		{name: "version", args: []string{cmdVersion}, expectedCode: exitOK, expectedStdout: "rig "},
		{name: "help command", args: []string{cmdHelp}, expectedCode: exitOK, expectedStdout: "Usage: rig"},
		{name: "help flag", args: []string{"-h"}, expectedCode: exitOK, expectedStderr: "Usage: rig"},
		{name: "no command", args: nil, expectedCode: exitError, expectedStderr: "Usage: rig"},
		{name: "unknown command", args: []string{"nope"}, expectedCode: exitError, expectedStderr: `rig: unknown command "nope"`},
		{name: "unknown flag", args: []string{"-nope", cmdVersion}, expectedCode: exitError, expectedStderr: "flag provided but not defined"},
		{
			name:           "invalid log format",
			args:           []string{"-log-format", "xml", cmdVersion},
			expectedCode:   exitError,
			expectedStderr: `rig: invalid log format "xml"`,
		},
		{
			name:           "invalid log level",
			args:           []string{"-log-level", "loud", cmdVersion},
			expectedCode:   exitError,
			expectedStderr: `rig: invalid log level "loud"`,
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

func TestRun_JSONLog(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer

	code := Run(t.Context(), []string{"-log-format", "json", "-log-level", "debug", cmdVersion}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("Run() = %d, want %d (stderr %q)", code, exitOK, stderr.String())
	}

	var record struct {
		Level   string `json:"level"`
		Msg     string `json:"msg"`
		Command string `json:"command"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &record); err != nil {
		t.Fatalf("standard error %q is not one JSON record: %v", stderr.String(), err)
	}

	if record.Level != "DEBUG" || record.Msg != "command started" || record.Command != cmdVersion {
		t.Errorf("record = %+v, want the debug record of the version command", record)
	}
}

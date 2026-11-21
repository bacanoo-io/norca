// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

const (
	redisDir = "../../examples/registry/modules/redis"
	flagVar  = "-var"
)

func TestRun_ModuleRender(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		args           []string
		expectedCode   int
		expectedStdout []string
		expectedStderr string
	}{
		{
			name:           "defaults",
			args:           []string{cmdModule, cmdRender, redisDir},
			expectedCode:   exitOK,
			expectedStdout: []string{`"release": "redis"`, `"kind": "job"`, `"ID": "redis"`, `"Region": "global"`},
		},
		{
			name: "values from a file and flags",
			args: []string{
				cmdModule, cmdRender, redisDir, "-f", redisDir + "/examples/auth.vars.hcl",
				flagVar, "count=2", "-release", "cache", "-namespace", "apps", "-region", "eu",
			},
			expectedCode: exitOK,
			expectedStdout: []string{
				`"kind": "variable"`, `"secret_items": [`, `"password"`, `"Count": 2`, `"Region": "eu"`,
			},
		},
		{
			name:           "flags before the directory",
			args:           []string{cmdModule, cmdRender, flagVar, "job_name=kv", redisDir},
			expectedCode:   exitOK,
			expectedStdout: []string{`"ID": "kv"`},
		},
		{
			name:           "an invalid value",
			args:           []string{cmdModule, cmdRender, redisDir, flagVar, "mode=cluster"},
			expectedCode:   exitError,
			expectedStderr: `var.mode is "cluster", not one of`,
		},
		{
			name:           "not name=value",
			args:           []string{cmdModule, cmdRender, redisDir, flagVar, "count"},
			expectedCode:   exitError,
			expectedStderr: `"count" is not name=value`,
		},
		{
			name:           "two directories",
			args:           []string{cmdModule, cmdRender, redisDir, redisDir},
			expectedCode:   exitError,
			expectedStderr: "module render takes one directory",
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
					t.Errorf("stdout = %s, want it to contain %s", stdout.String(), want)
				}
			}

			if strings.Contains(stdout.String(), "example-only") {
				t.Error("stdout holds the value of a secret item")
			}

			if !strings.Contains(stderr.String(), tt.expectedStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.expectedStderr)
			}

			if tt.expectedCode == exitOK && !json.Valid(stdout.Bytes()) {
				t.Errorf("stdout is not JSON: %s", stdout.String())
			}
		})
	}
}

func TestVarValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		text     string
		expected cty.Value
	}{
		{text: "3", expected: cty.NumberIntVal(3)},
		{text: "true", expected: cty.True},
		{text: `["a", "b"]`, expected: cty.TupleVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")})},
		{text: "redis", expected: cty.StringVal("redis")},
		{text: "eu-1", expected: cty.StringVal("eu-1")},
		{text: "", expected: cty.StringVal("")},
	}

	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			t.Parallel()

			if got := varValue(tt.text); !got.RawEquals(tt.expected) {
				t.Errorf("varValue(%q) = %#v, want %#v", tt.text, got, tt.expected)
			}
		})
	}
}

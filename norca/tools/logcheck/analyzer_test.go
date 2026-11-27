// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package logcheck

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), Analyzer, "a", "example.com/norca/helper/telemetry")
}

func TestAnalyzer_fixes(t *testing.T) {
	analysistest.RunWithSuggestedFixes(t, analysistest.TestData(), Analyzer, "fix")
}

func TestSnakeCase(t *testing.T) {
	tests := []struct {
		key      string
		expected string
	}{
		{key: "allocID", expected: "alloc_id"},
		{key: "plugin.type", expected: "plugin_type"},
		{key: "plugin-type", expected: "plugin_type"},
		{key: "IP", expected: "ip"},
		{key: "monitorPath", expected: "monitor_path"},
		{key: "cpuset.cpus", expected: "cpuset_cpus"},
		{key: "HTTPServer", expected: "http_server"},
		{key: "alloc_id", expected: "alloc_id"},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			if got := snakeCase(tt.key); got != tt.expected {
				t.Errorf("snakeCase(%q) = %q, want %q", tt.key, got, tt.expected)
			}
		})
	}
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
)

// The agent has no audit block: a configuration that still has one is
// refused like any other unknown key, in HCL and in JSON. The servers' audit
// records of operator calls are log records and need no block.
func TestConfig_AuditBlockIsUnknown(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		name    string
		file    string
		content string
	}{
		{
			name:    "hcl",
			file:    "agent.hcl",
			content: "audit {\n  enabled = true\n}\n",
		},
		{
			name:    "json",
			file:    "agent.json",
			content: `{"audit": {"enabled": true}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.file)
			must.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))

			_, err := ParseConfigFile(path)
			must.ErrorContains(t, err, "unexpected keys audit")
		})
	}
}

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

// The server block has no raft_protocol: Norca speaks Raft protocol 3 only,
// and a config that still sets it is refused rather than ignored.
func TestConfig_RaftProtocolIsUnknown(t *testing.T) {
	ci.Parallel(t)

	path := filepath.Join(t.TempDir(), "server.hcl")
	must.NoError(t, os.WriteFile(path, []byte("server {\n  raft_protocol = 3\n}\n"), 0o600))

	_, err := ParseConfigFile(path)
	must.ErrorContains(t, err, "unexpected keys raft_protocol")
}

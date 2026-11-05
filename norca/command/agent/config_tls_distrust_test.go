// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

// TestConfig_TLS_DistrustedCAFile checks that the distrusted CAs of the TLS
// block are read from the configuration and kept by a merge.
func TestConfig_TLS_DistrustedCAFile(t *testing.T) {
	ci.Parallel(t)

	hcl := `tls {
  rpc                = true
  distrusted_ca_file = "/etc/norca/tls/distrusted.pem"
}
`
	path := filepath.Join(t.TempDir(), "tls.hcl")
	must.NoError(t, os.WriteFile(path, []byte(hcl), 0o600))
	c, err := ParseConfigFile(path)
	must.NoError(t, err)
	must.Eq(t, "/etc/norca/tls/distrusted.pem", c.TLSConfig.DistrustedCAFile)

	merged := DefaultConfig().Merge(c)
	must.Eq(t, "/etc/norca/tls/distrusted.pem", merged.TLSConfig.DistrustedCAFile)
}

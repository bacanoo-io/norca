// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fingerprint

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/nomad/structs"
	structsc "github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/hashicorp/nomad/version"
	"github.com/shoenig/test/must"
)

// TestNomadFingerprint_VaultCompat checks the attribute of the clients that
// serve vault blocks from the built-in secret store.
func TestNomadFingerprint_VaultCompat(t *testing.T) {
	ci.Parallel(t)
	f := NewNomadFingerprint(testlog.HCLogger(t))
	attr := func(c *config.Config) (string, bool) {
		c.Version = &version.VersionInfo{Version: "1.6.5"}
		var resp FingerprintResponse
		must.NoError(t, f.Fingerprint(&FingerprintRequest{Config: c, Node: &structs.Node{}}, &resp))
		v, ok := resp.Attributes[config.AttrVaultCompat]
		return v, ok
	}

	v, ok := attr(&config.Config{})
	must.True(t, ok)
	must.Eq(t, "true", v)

	_, ok = attr(&config.Config{VaultConfig: &structsc.VaultConfig{Enabled: pointer.Of(true)}})
	must.False(t, ok)

	_, ok = attr(&config.Config{Secrets: &config.SecretsConfig{
		VaultCompat: &config.VaultCompatConfig{Enabled: pointer.Of(false)}}})
	must.False(t, ok)
}

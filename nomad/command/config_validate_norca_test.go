// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
)

// runConfigValidate runs config validate on a file with content and returns
// the exit code and the error output.
func runConfigValidate(t *testing.T, content string) (int, string) {
	t.Helper()
	dir := t.TempDir()
	must.NoError(t, os.WriteFile(filepath.Join(dir, "config.hcl"), []byte(content), 0o644))
	ui := cli.NewMockUi()
	cmd := &ConfigValidateCommand{Meta: Meta{Ui: ui}}
	code := cmd.Run([]string{dir})
	return code, ui.ErrorWriter.String()
}

// The values of the Norca blocks are checked as the agent checks them when
// it starts.
func TestConfigValidateCommand_NorcaBlockValues(t *testing.T) {
	ci.Parallel(t)

	const server = `data_dir = "/opt/norca"
server {
  enabled = true
%s
}
`
	const client = `data_dir = "/opt/norca"
client {
  enabled = true
%s
}
`
	cases := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name:    "planned seal",
			config:  `keyring { seal "awskms" {} }`,
			wantErr: `seal "awskms" is not implemented yet`,
		},
		{
			name:    "unknown seal",
			config:  `keyring { seal "magic" {} }`,
			wantErr: `unknown seal type "magic"`,
		},
		{
			name:    "seal shares",
			config:  `keyring { seal "shares" { shares = 3 threshold = 4 } }`,
			wantErr: "threshold must be between 2 and shares",
		},
		{
			name: "backup placeholder recipient",
			config: `backup {
  enabled = true
  destination "s3" { bucket = "b" }
  encryption { recipients = ["age1CHANGE_ME"] }
}`,
			wantErr: "invalid age recipient",
		},
		{
			name: "backup without destination",
			config: `backup {
  enabled = true
}`,
			wantErr: "at least one destination is required",
		},
		{
			name:    "backup schedule",
			config:  `backup { schedule = "every hour" }`,
			wantErr: "invalid schedule",
		},
		{
			name:    "autoscaler duration",
			config:  `autoscaler { min_cooldown = "soon" }`,
			wantErr: "autoscaler.min_cooldown can't parse time duration",
		},
		{
			name:    "autoscaler engine",
			config:  `autoscaler { default_engine = "magic" }`,
			wantErr: "invalid autoscaler configuration",
		},
		{
			name:    "mtls ttl",
			config:  `mtls { leaf_cert_ttl = "24h" ca_cert_ttl = "48h" }`,
			wantErr: "ca_cert_ttl must be at least ten times leaf_cert_ttl",
		},
		{
			name:    "mtls trust domain",
			config:  `mtls { trust_domain = "Not A Domain" }`,
			wantErr: "invalid server mtls configuration",
		},
		{
			name:    "secrets audit",
			config:  `secrets { audit_file = "/var/log/audit.log" audit_rotate_duration = "daily" }`,
			wantErr: "secrets.audit_rotate_duration",
		},
		{
			name:    "discovery default overflow fraction",
			config:  `native_service_discovery { default_overflow_fraction = 1.5 }`,
			wantErr: "default_overflow_fraction must be between 0 and 1",
		},
		{
			name:    "discovery unknown key",
			config:  `native_service_discovery { overflow_fraction = 0.3 }`,
			wantErr: "overflow_fraction",
		},
	}
	for _, tc := range cases {
		t.Run("server "+tc.name, func(t *testing.T) {
			code, errOut := runConfigValidate(t, fmt.Sprintf(server, tc.config))
			must.One(t, code, must.Sprint(errOut))
			must.StrContains(t, errOut, tc.wantErr)
		})
	}

	clientCases := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name:    "dns bind",
			config:  `dns { enabled = true bind = "nowhere" }`,
			wantErr: "invalid dns config",
		},
		{
			name:    "consul_compat datacenter_from",
			config:  `consul_compat { enabled = true datacenter_from = "zone" }`,
			wantErr: "consul_compat datacenter_from must be",
		},
		{
			name:    "vault_compat token_ttl",
			config:  `secrets { vault_compat { token_ttl = "forever" } }`,
			wantErr: "vault_compat token_ttl",
		},
		{
			name:    "vault route without vault",
			config:  `secrets { route "kv/" { provider = "vault" } }`,
			wantErr: "invalid client secrets config",
		},
	}
	for _, tc := range clientCases {
		t.Run("client "+tc.name, func(t *testing.T) {
			code, errOut := runConfigValidate(t, fmt.Sprintf(client, tc.config))
			must.One(t, code, must.Sprint(errOut))
			must.StrContains(t, errOut, tc.wantErr)
		})
	}

	// valid values pass
	code, errOut := runConfigValidate(t, fmt.Sprintf(server, `keyring { seal "shares" { shares = 5 threshold = 3 } }
  autoscaler { enabled = true default_engine = "builtin" }
  mtls { leaf_cert_ttl = "24h" }
  native_service_discovery { default_overflow_fraction = 0 }
  backup {
    enabled = true
    destination "s3" { bucket = "b" }
    encryption { recipients = ["age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p"] }
  }`))
	must.Zero(t, code, must.Sprint(errOut))

	// the blocks of a disabled server are not checked, as by the agent
	code, errOut = runConfigValidate(t, `data_dir = "/opt/norca"
client { enabled = true }
server {
  enabled = false
  keyring { seal "awskms" {} }
}
`)
	must.Zero(t, code, must.Sprint(errOut))
}

// The agent warns when its consul block points at its own Consul-compatible
// API.
func TestConfigValidateCommand_ConsulCompatLoopWarning(t *testing.T) {
	ci.Parallel(t)

	dir := t.TempDir()
	must.NoError(t, os.WriteFile(filepath.Join(dir, "config.hcl"), []byte(`data_dir = "/opt/norca"
client {
  enabled = true
  consul_compat { enabled = true }
}
consul { address = "127.0.0.1:8500" }
`), 0o644))
	ui := cli.NewMockUi()
	cmd := &ConfigValidateCommand{Meta: Meta{Ui: ui}}
	must.Zero(t, cmd.Run([]string{dir}))
	must.StrContains(t, ui.ErrorWriter.String(), "consul.address points at client.consul_compat")
}

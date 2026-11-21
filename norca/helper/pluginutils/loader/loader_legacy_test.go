// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package loader

import (
	"testing"

	log "github.com/hashicorp/go-hclog"
	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/bacanoo-io/norca/norca/norca/structs/config"
	"github.com/bacanoo-io/norca/norca/plugins/base"
	"github.com/bacanoo-io/norca/norca/plugins/device"
)

// A driver or device plugin built against the plugin packages of Nomad 1.6.5
// looks for the magic cookie under the key of those packages; the loader sets
// it next to its own, so such a plugin is found and dispensed (review finding
// ASP-2).
func TestPluginLoader_External_NomadCookieKey(t *testing.T) {
	ci.Parallel(t)

	plugins := []string{"nomad-device", "norca-device"}
	h := newHarness(t, plugins)

	logger := testlog.HCLogger(t)
	logger.SetLevel(log.Trace)
	args := func(name string, extra ...string) []string {
		return append([]string{"-plugin", "-name", name, "-type", base.PluginTypeDevice,
			"-version", "v0.0.1", "-api-version", device.ApiVersion010}, extra...)
	}
	l, err := NewPluginLoader(&PluginLoaderConfig{
		Logger:            logger,
		PluginDir:         h.pluginDir(),
		SupportedVersions: supportedApiVersions,
		Configs: []*config.PluginConfig{
			{Name: plugins[0], Args: args(plugins[0], "-nomad-key")},
			{Name: plugins[1], Args: args(plugins[1])},
		},
	})
	must.NoError(t, err)

	detected := l.Catalog()[base.PluginTypeDevice]
	must.Len(t, 2, detected)

	for _, name := range plugins {
		p, err := l.Dispense(name, base.PluginTypeDevice, nil, logger)
		must.NoError(t, err, must.Sprint(name))
		info, err := p.Plugin().(base.BasePlugin).PluginInfo()
		must.NoError(t, err, must.Sprint(name))
		must.Eq(t, name, info.Name)
		p.Kill()
	}
}

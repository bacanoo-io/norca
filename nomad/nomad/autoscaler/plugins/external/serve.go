// Copyright IBM Corp. 2020, 2026
// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Ported from Nomad Autoscaler v0.6.0 plugins/plugin.go and
// plugins/manager/util.go. Modified: target plugins are not served or
// dispensed yet (node scaling is a later phase).

// Package external hosts the external plugins of the built-in autoscaler:
// APM and strategy plugins built with the Nomad Autoscaler plugin SDK, which
// run as separate processes and talk to the server over go-plugin's gRPC
// protocol. The handshake, the gRPC services and the messages are the ones of
// the Nomad Autoscaler v0.6.0, so existing plugin binaries run unchanged.
package external

import (
	hclog "github.com/hashicorp/go-hclog"
	plugin "github.com/hashicorp/go-plugin"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins"
	"github.com/hashicorp/nomad/nomad/autoscaler/sdk"
)

// Handshake is used to do a basic handshake between a plugin and host. If
// the handshake fails, a user friendly error is shown. This prevents users
// from executing bad plugins or executing a plugin directory. It is a UX
// feature, not a security feature.
//
// The values are those of the Nomad Autoscaler, whose plugin versioning is
// performed using the ProtocolVersion within the Handshake.
var Handshake = plugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "NOMAD_AUTOSCALER_PLUGIN_MAGIC_COOKIE",
	MagicCookieValue: "e082fa04d587a6525d683666fa253d6afda00f20c122c54a80a3ed57fec99ff3",
}

// Serve is used by a plugin binary to serve an APM or strategy plugin. The
// factory returns a plugins.APM or a plugins.Strategy.
func Serve(f plugins.PluginFactory) {
	logger := hclog.New(&hclog.LoggerOptions{
		Level:      hclog.Trace,
		JSONFormat: true,
	})

	p := f(logger)
	if p == nil {
		logger.Error("plugin factory returned nil")
		return
	}

	pCfg := plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Logger:          logger,
		GRPCServer:      plugin.DefaultGRPCServer,
	}

	switch impl := p.(type) {
	case plugins.APM:
		pCfg.Plugins = map[string]plugin.Plugin{
			sdk.PluginTypeAPM:  &PluginAPM{Impl: impl},
			sdk.PluginTypeBase: &PluginBase{Impl: impl},
		}
	case plugins.Strategy:
		pCfg.Plugins = map[string]plugin.Plugin{
			sdk.PluginTypeStrategy: &PluginStrategy{Impl: impl},
			sdk.PluginTypeBase:     &PluginBase{Impl: impl},
		}
	default:
		logger.Error("unsupported plugin type", "type", hclog.Fmt("%T", p))
		return
	}

	plugin.Serve(&pCfg)
}

// pluginMap returns the go-plugin plugin map used to dispense a plugin of
// the given type.
func pluginMap(pluginType string) map[string]plugin.Plugin {
	// All plugins implement the base.
	m := map[string]plugin.Plugin{sdk.PluginTypeBase: &PluginBase{}}

	switch pluginType {
	case sdk.PluginTypeAPM:
		m[pluginType] = &PluginAPM{}
	case sdk.PluginTypeStrategy:
		m[pluginType] = &PluginStrategy{}
	}
	return m
}

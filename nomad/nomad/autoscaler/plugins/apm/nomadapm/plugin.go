// Copyright IBM Corp. 2020, 2026
// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Ported from Nomad Autoscaler v0.6.0 plugins/builtin/apm/nomad/plugin/plugin.go.
// Modified: instead of an HTTP API client the plugin reads the server state
// store and calls the internal ClientAllocations.Stats RPC. This per
// allocation RPC is transitional (phase 1 of the built-in autoscaler); it is
// replaced by client side metric push in a later phase.

// Package nomadapm is the built-in "nomad-apm" APM of the built-in
// autoscaler.
package nomadapm

import (
	hclog "github.com/hashicorp/go-hclog"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/nomad/autoscaler/plugins"
	"github.com/hashicorp/nomad/nomad/autoscaler/sdk"
	"github.com/hashicorp/nomad/nomad/state"
)

const (
	// pluginName is the name of the plugin
	pluginName = plugins.InternalAPMNomad
)

var (
	PluginID = plugins.PluginID{
		Name:       pluginName,
		PluginType: sdk.PluginTypeAPM,
	}

	pluginInfo = &plugins.PluginInfo{
		Name:       pluginName,
		PluginType: sdk.PluginTypeAPM,
	}
)

// StateFn returns the current server state store.
type StateFn func() *state.StateStore

// AllocStatsFn returns the current resource usage of an allocation. On the
// server it is backed by the ClientAllocations.Stats RPC.
type AllocStatsFn func(namespace, allocID string) (*cstructs.AllocResourceUsage, error)

// Assert that APMPlugin meets the plugins.APM interface.
var _ plugins.APM = (*APMPlugin)(nil)

// APMPlugin is the in-process Nomad APM.
type APMPlugin struct {
	logger     hclog.Logger
	state      StateFn
	allocStats AllocStatsFn
}

// NewNomadPlugin returns a new in-process Nomad APM.
func NewNomadPlugin(log hclog.Logger, state StateFn, allocStats AllocStatsFn) *APMPlugin {
	return &APMPlugin{
		logger:     log,
		state:      state,
		allocStats: allocStats,
	}
}

// SetConfig satisfies the SetConfig function on the plugins.Base interface.
// The in-process plugin has no configuration: it inherits the server's view
// of the cluster.
func (a *APMPlugin) SetConfig(_ map[string]string) error {
	return nil
}

// PluginInfo satisfies the PluginInfo function on the plugins.Base interface.
func (a *APMPlugin) PluginInfo() (*plugins.PluginInfo, error) {
	return pluginInfo, nil
}

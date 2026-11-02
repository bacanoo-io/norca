// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import pstructs "github.com/bacanoo-io/norca/norca/plugins/shared/structs"

// PluginState is used to store the driver manager's state across restarts of the
// agent
type PluginState struct {
	// ReattachConfigs are the set of reattach configs for plugins launched by
	// the driver manager
	ReattachConfigs map[string]*pstructs.ReattachConfig
}

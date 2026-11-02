// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import "github.com/bacanoo-io/norca/norca/client/dynamicplugins"

// RegistryState12 is the dynamic plugin registry state persisted
// before 1.3.0.
type RegistryState12 struct {
	Plugins map[string]map[string]*dynamicplugins.PluginInfo
}

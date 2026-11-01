// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package catalog

import (
	"github.com/bacanoo-io/norca/nomad/drivers/docker"
	"github.com/bacanoo-io/norca/nomad/drivers/exec"
	"github.com/bacanoo-io/norca/nomad/drivers/java"
	"github.com/bacanoo-io/norca/nomad/drivers/qemu"
	"github.com/bacanoo-io/norca/nomad/drivers/rawexec"
)

// This file is where all builtin plugins should be registered in the catalog.
// Plugins with build restrictions should be placed in the appropriate
// register_XXX.go file.
func init() {
	RegisterDeferredConfig(rawexec.PluginID, rawexec.PluginConfig, rawexec.PluginLoader)
	Register(exec.PluginID, exec.PluginConfig)
	Register(qemu.PluginID, qemu.PluginConfig)
	Register(java.PluginID, java.PluginConfig)
	RegisterDeferredConfig(docker.PluginID, docker.PluginConfig, docker.PluginLoader)
}

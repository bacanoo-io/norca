// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package catalog

import (
	exec2 "github.com/bacanoo-io/norca/norca/drivers/exec2/plugin"
)

// The exec2 driver uses Linux only features (Landlock, cgroups v2, clone
// into a cgroup), so it is only built on Linux.
func init() {
	Register(exec2.PluginID, exec2.PluginConfig)
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/plugins/drivers"
	"github.com/bacanoo-io/norca/norca/plugins/drivers/fsisolation"
	"github.com/shoenig/test/must"
)

// TestCSIPluginSupervisorHook_StagePublishDir checks the directory the client
// hands a plugin for staging and publishing: stage_publish_base_dir for a task
// with its own file system, the plugin directory of the host for a task that
// sees the host paths (raw_exec), where nothing is mounted at
// stage_publish_base_dir.
func TestCSIPluginSupervisorHook_StagePublishDir(t *testing.T) {
	ci.Parallel(t)

	for mode, want := range map[fsisolation.Mode]string{
		fsisolation.None:   "/data/csi/node/p",
		fsisolation.Unveil: "/data/csi/node/p",
		fsisolation.Chroot: "/local/csi",
		fsisolation.Image:  "/local/csi",
	} {
		h := &csiPluginSupervisorHook{
			mountPoint: "/data/csi/node/p",
			task:       &structs.Task{CSIPluginConfig: &structs.TaskCSIPluginConfig{StagePublishBaseDir: "/local/csi"}},
			caps:       &drivers.Capabilities{FSIsolation: mode},
		}
		must.Eq(t, want, h.pluginStagePublishDir(), must.Sprintf("mode %s", mode))
	}
}

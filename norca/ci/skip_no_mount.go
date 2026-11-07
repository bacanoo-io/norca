// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package ci

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// capSysAdmin is the bit of CAP_SYS_ADMIN in the capability sets of
// /proc/self/status.
const capSysAdmin = 21

// SkipWithoutMount skips a test whose task must start on a client of the
// test agent when that client would fail to start it: on Linux a client
// running as root mounts a tmpfs on the secrets dir of every task, and
// without CAP_SYS_ADMIN (a container started without --cap-add SYS_ADMIN)
// the task_dir hook fails with "mount: operation not permitted". A client
// that is not root does not mount, so the test runs there.
func SkipWithoutMount(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skipf("test requires CAP_SYS_ADMIN as root, the capabilities can't be read: %v", err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		hex, ok := strings.CutPrefix(line, "CapEff:")
		if !ok {
			continue
		}
		caps, err := strconv.ParseUint(strings.TrimSpace(hex), 16, 64)
		if err != nil {
			t.Skipf("test requires CAP_SYS_ADMIN as root, the capabilities can't be parsed: %v", err)
		}
		if caps&(1<<capSysAdmin) == 0 {
			t.Skip("test requires CAP_SYS_ADMIN as root to mount the task secrets dir (in a container: --cap-add SYS_ADMIN)")
		}
		return
	}
	t.Skip("test requires CAP_SYS_ADMIN as root, CapEff not found in /proc/self/status")
}

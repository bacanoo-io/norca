// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package plugin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bacanoo-io/norca/norca/client/lib/cgutil"
	"github.com/shoenig/test/must"
)

// testSlice is where the driver test harness makes the task cgroups.
const testSlice = "testing.slice"

// testCgroupPath is the cgroup the driver harness makes for a task, the one
// the client's cpuset manager would otherwise make under its own slice.
func testCgroupPath(allocID, task string) string {
	return filepath.Join(cgutil.CgroupRoot, testSlice, cgutil.CgroupScope(allocID, task))
}

// initTestCgroups prepares the test slice the way the client prepares its
// own: the controllers the driver writes to are enabled for the task cgroups
// below it, and the slice may use the given cores.
func initTestCgroups(t *testing.T, cores string) {
	t.Helper()
	if !cgutil.UseV2 {
		t.Skip("exec2 needs cgroups v2")
	}

	slice := filepath.Join(cgutil.CgroupRoot, testSlice)
	must.NoError(t, os.MkdirAll(slice, 0o755))

	enable := []byte("+cpuset +cpu +memory +pids")
	for _, dir := range []string{cgutil.CgroupRoot, slice} {
		must.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), enable, 0o644))
	}
	must.NoError(t, os.WriteFile(filepath.Join(slice, "cpuset.cpus"), []byte(cores), 0o644))
}

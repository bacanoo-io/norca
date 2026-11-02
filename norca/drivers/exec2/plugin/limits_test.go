// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	ctests "github.com/bacanoo-io/norca/norca/client/testutil"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/plugins/drivers"
	"github.com/shoenig/test/must"
)

func TestResolvePidsLimit(t *testing.T) {
	cases := []struct {
		name         string
		plugin, task int64
		exp          int64
		err          bool
	}{
		{name: "none", exp: 0},
		{name: "plugin only", plugin: 100, exp: 100},
		{name: "task only", task: 50, exp: 50},
		{name: "task lower", plugin: 100, task: 50, exp: 50},
		{name: "task equal", plugin: 100, task: 100, exp: 100},
		{name: "task higher", plugin: 100, task: 200, err: true},
		{name: "negative", task: -1, err: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolvePidsLimit(tc.plugin, tc.task)
			if tc.err {
				must.Error(t, err)
				return
			}
			must.NoError(t, err)
			must.Eq(t, tc.exp, got)
		})
	}
}

// TestFunctional_limits starts a task and checks the limits in its cgroup.
func TestFunctional_limits(t *testing.T) {
	ctests.RequireRoot(t)

	cases := []struct {
		name     string
		plugin   *Config
		task     int64
		pidsMax  string
		swapMax  string
		startErr bool
	}{
		{name: "defaults", plugin: &Config{UnveilDefaults: true}, pidsMax: "max", swapMax: "0"},
		{name: "plugin pids limit", plugin: &Config{UnveilDefaults: true, PidsLimit: 64}, pidsMax: "64", swapMax: "0"},
		{name: "task lowers it", plugin: &Config{UnveilDefaults: true, PidsLimit: 64}, task: 32, pidsMax: "32", swapMax: "0"},
		{name: "task above plugin", plugin: &Config{UnveilDefaults: true, PidsLimit: 64}, task: 128, startErr: true},
		{name: "swap allowed", plugin: &Config{UnveilDefaults: true, AllowSwap: true}, pidsMax: "max", swapMax: "max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allocID := uuid.Generate()
			taskName := "limits_" + uuid.Short()
			task := &drivers.TaskConfig{
				User:      "norca:80000",
				ID:        uuid.Generate(),
				Name:      taskName,
				AllocID:   allocID,
				Resources: basicResources(allocID, taskName),
			}
			must.NoError(t, task.EncodeConcreteDriverConfig(&TaskConfig{
				Command:   "sleep",
				Args:      []string{"infinity"},
				PidsLimit: tc.task,
			}))

			harness := newTestHarness(t, tc.plugin)
			cleanup := harness.MkAllocDir(task, true)
			defer cleanup()

			_, _, err := harness.StartTask(task)
			if tc.startErr {
				must.ErrorContains(t, err, "pids_limit")
				return
			}
			must.NoError(t, err)
			defer func() { _ = harness.DestroyTask(task.ID, true) }()

			cg := testCgroupPath(allocID, taskName)
			read := func(file string) string {
				b, err := os.ReadFile(filepath.Join(cg, file))
				must.NoError(t, err)
				return strings.TrimSpace(string(b))
			}
			must.Eq(t, tc.pidsMax, read("pids.max"))
			must.Eq(t, "1", read("memory.oom.group"))
			if _, err := os.Stat(filepath.Join(cg, "memory.swap.max")); err == nil {
				must.Eq(t, tc.swapMax, read("memory.swap.max"))
			}
		})
	}
}

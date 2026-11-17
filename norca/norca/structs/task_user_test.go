// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

// TestTask_Validate_DynamicUserName checks that a job cannot name the user
// of its task in the form the clients give to dynamic users ("norca:<id>"):
// a driver would run the task with that ID, which may be the dynamic user of
// another task or any user of the node.
func TestTask_Validate_DynamicUserName(t *testing.T) {
	ci.Parallel(t)

	tg := &TaskGroup{EphemeralDisk: DefaultEphemeralDisk()}
	tg.EphemeralDisk.SizeMB = 200
	task := func(user string) *Task {
		return &Task{
			Name:      "web",
			Driver:    "exec2",
			User:      user,
			Resources: &Resources{CPU: 100, MemoryMB: 100},
			LogConfig: DefaultLogConfig(),
		}
	}

	for _, user := range []string{"norca:85000", "norca:1"} {
		err := task(user).Validate(JobTypeService, tg)
		must.Error(t, err, must.Sprint(user))
		must.StrContains(t, err.Error(), "is reserved for the dynamic users")
	}
	for _, user := range []string{"", "nobody", "norca", "norca:staff", "1000:1000"} {
		must.NoError(t, task(user).Validate(JobTypeService, tg), must.Sprint(user))
	}
}

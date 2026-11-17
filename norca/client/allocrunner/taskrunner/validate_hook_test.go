// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/client/config"
	"github.com/bacanoo-io/norca/norca/client/taskenv"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/stretchr/testify/require"
)

func TestTaskRunner_Validate_UserEnforcement(t *testing.T) {
	ci.Parallel(t)

	taskEnv := taskenv.NewEmptyBuilder().Build()
	conf := config.DefaultConfig()

	// Try to run as root with exec.
	task := &structs.Task{
		Driver: "exec",
		User:   "root",
	}
	if err := validateTask(task, taskEnv, conf); err == nil {
		t.Fatalf("expected error running as root with exec")
	}

	// Try to run a non-blacklisted user with exec.
	task.User = "foobar"
	require.NoError(t, validateTask(task, taskEnv, conf))

	// Try to run as root with docker.
	task.Driver = "docker"
	task.User = "root"
	require.NoError(t, validateTask(task, taskEnv, conf))
}

func TestTaskRunner_Validate_ServiceName(t *testing.T) {
	ci.Parallel(t)

	builder := taskenv.NewEmptyBuilder()
	conf := config.DefaultConfig()

	// Create a task with a service for validation
	task := &structs.Task{
		Services: []*structs.Service{
			{
				Name: "ok",
			},
		},
	}

	require.NoError(t, validateTask(task, builder.Build(), conf))

	// Add an env var that should validate
	builder.SetHookEnv("test", map[string]string{"FOO": "bar"})
	task.Services[0].Name = "${FOO}"
	require.NoError(t, validateTask(task, builder.Build(), conf))

	// Add an env var that should *not* validate
	builder.SetHookEnv("test", map[string]string{"BAD": "invalid/in/consul"})
	task.Services[0].Name = "${BAD}"
	require.Error(t, validateTask(task, builder.Build(), conf))
}

// TestTaskRunner_Validate_DynamicUserName checks that the user a job gives
// its task is refused when it has the form of the names the client gives to
// dynamic users: with any driver, since the drivers turn such a name into
// the ID in it.
func TestTaskRunner_Validate_DynamicUserName(t *testing.T) {
	ci.Parallel(t)

	for _, user := range []string{"norca:85000", "norca:1"} {
		err := validateJobUser(user)
		require.ErrorContains(t, err, "is reserved for the dynamic users", user)
	}
	for _, user := range []string{"", "nobody", "norca", "norca:staff", "1000:1000"} {
		require.NoError(t, validateJobUser(user), user)
	}
}

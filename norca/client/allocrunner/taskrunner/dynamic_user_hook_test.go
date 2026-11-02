// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"context"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/client/allocrunner/interfaces"
	"github.com/bacanoo-io/norca/norca/client/allocrunner/taskrunner/state"
	"github.com/bacanoo-io/norca/norca/client/config"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/bacanoo-io/norca/norca/helper/users/dynamic"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/plugins/drivers"
	"github.com/shoenig/test/must"
)

// dynamicUserRunner is the part of a TaskRunner the dynamic user hook uses.
func dynamicUserRunner(t *testing.T, user string, dynamicUsers bool, pool dynamic.Pool) *TaskRunner {
	return &TaskRunner{
		task:               &structs.Task{Name: "web", Driver: "exec2", User: user},
		driverCapabilities: &drivers.Capabilities{DynamicWorkloadUsers: dynamicUsers},
		clientConfig:       &config.Config{DynamicUserPool: pool},
		localState:         state.NewLocalState(),
		logger:             testlog.HCLogger(t),
	}
}

func TestDynamicUserHook_usesDynamicUser(t *testing.T) {
	ci.Parallel(t)
	pool := dynamic.New(dynamic.PoolConfig{Min: 100, Max: 101})

	must.True(t, dynamicUserRunner(t, "", true, pool).usesDynamicUser())
	must.False(t, dynamicUserRunner(t, "nobody", true, pool).usesDynamicUser(), must.Sprint("task names a user"))
	must.False(t, dynamicUserRunner(t, "", false, pool).usesDynamicUser(), must.Sprint("driver does not support it"))
}

func TestDynamicUserHook_lifecycle(t *testing.T) {
	ci.Parallel(t)
	pool := dynamic.New(dynamic.PoolConfig{Min: 100, Max: 101})
	tr := dynamicUserRunner(t, "", true, pool)
	h := newDynamicUserHook(tr, tr.logger)

	// prestart hands out a user and saves it
	resp := new(interfaces.TaskPrestartResponse)
	must.NoError(t, h.Prestart(context.Background(), &interfaces.TaskPrestartRequest{}, resp))
	must.True(t, resp.Done)
	must.Eq(t, map[string]string{dynamicUserStateKey: "100"}, resp.State)
	must.Eq(t, "norca:100", tr.taskUser())
	must.Eq(t, "norca:100", tr.taskWithUser().User)
	must.Eq(t, "", tr.Task().User, must.Sprint("the task itself is not changed"))

	// a second prestart (task restart) keeps the same user
	resp = new(interfaces.TaskPrestartResponse)
	must.NoError(t, h.Prestart(context.Background(), &interfaces.TaskPrestartRequest{}, resp))
	must.Eq(t, "norca:100", tr.taskUser())

	// stop frees it, twice is harmless
	must.NoError(t, h.Stop(context.Background(), nil, nil))
	must.NoError(t, h.Stop(context.Background(), nil, nil))
	must.Eq(t, "", tr.taskUser())

	// both IDs are free again
	a, err := pool.Acquire()
	must.NoError(t, err)
	b, err := pool.Acquire()
	must.NoError(t, err)
	must.SliceContainsAll(t, []dynamic.UGID{100, 101}, []dynamic.UGID{a, b})
}

func TestDynamicUserHook_restore(t *testing.T) {
	ci.Parallel(t)
	pool := dynamic.New(dynamic.PoolConfig{Min: 100, Max: 101})
	tr := dynamicUserRunner(t, "", true, pool)

	// the state a previous client process saved
	tr.localState.Hooks[dynamicUserHookName] = &state.HookState{
		PrestartDone: true,
		Data:         map[string]string{dynamicUserStateKey: "101"},
	}
	tr.restoreDynamicUser()
	must.Eq(t, "norca:101", tr.taskUser())

	// the restored ID is not handed out to another task
	id, err := pool.Acquire()
	must.NoError(t, err)
	must.Eq(t, dynamic.UGID(100), id)
	_, err = pool.Acquire()
	must.ErrorIs(t, err, dynamic.ErrPoolExhausted)
}

func TestDynamicUserHook_namedUser(t *testing.T) {
	ci.Parallel(t)
	tr := dynamicUserRunner(t, "nobody", true, nil)
	must.Eq(t, "nobody", tr.taskUser())
	must.Eq(t, "nobody", tr.taskWithUser().User)
}

func TestDynamicUserHook_noPool(t *testing.T) {
	ci.Parallel(t)
	tr := dynamicUserRunner(t, "", true, nil)
	h := newDynamicUserHook(tr, tr.logger)
	err := h.Prestart(context.Background(), &interfaces.TaskPrestartRequest{}, new(interfaces.TaskPrestartResponse))
	must.ErrorContains(t, err, "no dynamic user pool")
}

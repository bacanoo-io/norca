// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"context"
	"errors"
	"strconv"
	"sync"

	"github.com/bacanoo-io/norca/norca/client/allocrunner/interfaces"
	"github.com/bacanoo-io/norca/norca/helper/users/dynamic"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	hclog "github.com/hashicorp/go-hclog"
)

const (
	// dynamicUserHookName is the name of the hook and of its saved state.
	dynamicUserHookName = "dynamic_user"

	// dynamicUserStateKey holds the handed out ID in the hook state.
	dynamicUserStateKey = "ugid"
)

// dynamicUser is the anonymous user a task runs as, shared by the hook and
// the task runner (which passes it to the driver).
type dynamicUser struct {
	lock sync.Mutex
	id   dynamic.UGID // 0 while none is held
}

func (d *dynamicUser) get() dynamic.UGID {
	d.lock.Lock()
	defer d.lock.Unlock()
	return d.id
}

func (d *dynamicUser) set(id dynamic.UGID) {
	d.lock.Lock()
	defer d.lock.Unlock()
	d.id = id
}

// usesDynamicUser reports whether the task runs as a dynamic user: its
// driver supports them and the task does not name a user.
func (tr *TaskRunner) usesDynamicUser() bool {
	return tr.driverCapabilities != nil && tr.driverCapabilities.DynamicWorkloadUsers && tr.Task().User == ""
}

// taskUser is the user the driver runs the task as: the user of the task, or
// the synthetic name of its dynamic user.
func (tr *TaskRunner) taskUser() string {
	if user := tr.Task().User; user != "" {
		return user
	}
	if id := tr.dynamicUser.get(); id != 0 {
		return dynamic.String(id)
	}
	return ""
}

// restoreDynamicUser takes back the ID a task held before the client
// restarted, so that the pool does not hand it out to another task while
// this one may still be running. Called by Restore.
func (tr *TaskRunner) restoreDynamicUser() {
	hs := tr.hookState(dynamicUserHookName)
	if hs == nil || hs.Data[dynamicUserStateKey] == "" {
		return
	}
	n, err := strconv.Atoi(hs.Data[dynamicUserStateKey])
	if err != nil || n <= 0 {
		tr.logger.Warn("ignoring invalid saved dynamic user", "value", hs.Data[dynamicUserStateKey])
		return
	}
	id := dynamic.UGID(n)
	if pool := tr.clientConfig.DynamicUserPool; pool != nil {
		pool.Restore(id)
	}
	tr.dynamicUser.set(id)
}

// dynamicUserHook hands out a dynamic user before the task first starts and
// frees it when the task will not run again.
type dynamicUserHook struct {
	tr     *TaskRunner
	pool   dynamic.Pool
	logger hclog.Logger
}

func newDynamicUserHook(tr *TaskRunner, logger hclog.Logger) *dynamicUserHook {
	h := &dynamicUserHook{tr: tr, pool: tr.clientConfig.DynamicUserPool}
	h.logger = logger.Named(h.Name())
	return h
}

func (*dynamicUserHook) Name() string {
	return dynamicUserHookName
}

func (h *dynamicUserHook) Prestart(_ context.Context, req *interfaces.TaskPrestartRequest, resp *interfaces.TaskPrestartResponse) error {
	if h.pool == nil {
		return errors.New("the driver runs tasks as dynamic users, but the client has no dynamic user pool")
	}

	id := h.tr.dynamicUser.get()
	if id == 0 {
		var err error
		if id, err = h.pool.Acquire(); err != nil {
			return err
		}
		h.tr.dynamicUser.set(id)
		h.logger.Debug("handed out dynamic user", "ugid", id)
	}

	resp.State = map[string]string{dynamicUserStateKey: strconv.Itoa(int(id))}
	resp.Done = true
	return nil
}

func (h *dynamicUserHook) Stop(context.Context, *interfaces.TaskStopRequest, *interfaces.TaskStopResponse) error {
	id := h.tr.dynamicUser.get()
	if id == 0 {
		return nil
	}
	if h.pool != nil {
		h.pool.Release(id)
	}
	h.tr.dynamicUser.set(0)
	h.logger.Debug("released dynamic user", "ugid", id)
	return nil
}

// taskWithUser is the task as the hooks see it: when it runs as a dynamic
// user, a copy with that user set, so that files the hooks write for the task
// user belong to it.
func (tr *TaskRunner) taskWithUser() *structs.Task {
	task := tr.Task()
	if task.User != "" {
		return task
	}
	id := tr.dynamicUser.get()
	if id == 0 {
		return task
	}
	t := *task
	t.User = dynamic.String(id)
	return &t
}

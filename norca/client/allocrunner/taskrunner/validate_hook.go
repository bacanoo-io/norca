// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"context"
	"fmt"

	"github.com/bacanoo-io/norca/norca/client/allocrunner/interfaces"
	"github.com/bacanoo-io/norca/norca/client/config"
	"github.com/bacanoo-io/norca/norca/client/taskenv"
	"github.com/bacanoo-io/norca/norca/helper/users"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	log "github.com/hashicorp/go-hclog"
	multierror "github.com/hashicorp/go-multierror"
)

// validateHook validates the task is able to be run.
type validateHook struct {
	config *config.Config
	logger log.Logger

	// jobUser returns the user the job gives the task. The task of a
	// prestart request is not that: it carries the dynamic user of a task
	// without a user.
	jobUser func() string
}

func newValidateHook(config *config.Config, jobUser func() string, logger log.Logger) *validateHook {
	h := &validateHook{
		config:  config,
		jobUser: jobUser,
	}
	h.logger = logger.Named(h.Name())
	return h
}

func (*validateHook) Name() string {
	return "validate"
}

func (h *validateHook) Prestart(ctx context.Context, req *interfaces.TaskPrestartRequest, resp *interfaces.TaskPrestartResponse) error {
	if h.jobUser != nil {
		if err := validateJobUser(h.jobUser()); err != nil {
			return err
		}
	}
	if err := validateTask(req.Task, req.TaskEnv, h.config); err != nil {
		return err
	}

	resp.Done = true
	return nil
}

// validateJobUser refuses a user of a job that has the form of the names of
// dynamic users. The client gives those to tasks without a user, for an ID
// of its pool; the drivers turn such a name into the ID in it, so from a job
// it would run the task as the dynamic user of another task, or as any user
// of the node, past the user denylist.
func validateJobUser(user string) error {
	if users.IsDynamicName(user) {
		return fmt.Errorf("running as user %q is disallowed: the name is reserved for the dynamic users of tasks without a user", user)
	}
	return nil
}

func validateTask(task *structs.Task, taskEnv *taskenv.TaskEnv, conf *config.Config) error {
	var mErr multierror.Error

	// Validate the user
	// COMPAT(1.0) uses inclusive language. blacklist is kept for backward compatilibity.
	unallowedUsers := conf.ReadStringListAlternativeToMapDefault(
		[]string{"user.denylist", "user.blacklist"},
		config.DefaultUserDenylist,
	)
	checkDrivers := conf.ReadStringListToMapDefault("user.checked_drivers", config.DefaultUserCheckedDrivers)
	if _, driverMatch := checkDrivers[task.Driver]; driverMatch {
		if _, unallowed := unallowedUsers[task.User]; unallowed {
			mErr.Errors = append(mErr.Errors, fmt.Errorf("running as user %q is disallowed", task.User))
		}
	}

	// Validate the Service names once they're interpolated
	for _, service := range task.Services {
		name := taskEnv.ReplaceEnv(service.Name)
		if err := service.ValidateName(name); err != nil {
			mErr.Errors = append(mErr.Errors, fmt.Errorf("service (%s) failed validation: %v", name, err))
		}
	}

	if len(mErr.Errors) == 1 {
		return mErr.Errors[0]
	}
	return mErr.ErrorOrNil()
}

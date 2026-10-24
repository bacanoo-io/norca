// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"strings"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad/nomad/structs"
)

// jobQuotaMutatingHook fills in the task resources the job doesn't set from
// the task_defaults of the quota attached to the job's namespace (the
// equivalent of a Kubernetes LimitRange default).
type jobQuotaMutatingHook struct {
	srv *Server
}

func (jobQuotaMutatingHook) Name() string {
	return "quota-task-defaults"
}

func (h jobQuotaMutatingHook) Mutate(job *structs.Job) (*structs.Job, []error, error) {
	spec, err := jobQuotaSpec(h.srv, job)
	if err != nil {
		return nil, nil, err
	}
	if spec == nil || spec.TaskDefaults == nil {
		return job, nil, nil
	}

	for _, tg := range job.TaskGroups {
		for _, task := range tg.Tasks {
			applyQuotaTaskDefaults(task, spec.TaskDefaults)
		}
	}
	return job, nil, nil
}

// jobQuotaSpec returns the quota attached to the job namespace, if any. A
// server without state (as used by unit tests of the admission chain) has no
// quotas.
func jobQuotaSpec(srv *Server, job *structs.Job) (*structs.QuotaSpec, error) {
	if srv == nil || srv.fsm == nil {
		return nil, nil
	}
	return srv.State().QuotaForNamespace(nil, job.Namespace)
}

// quotaTaskResourceUnset returns whether a task resource value should be
// treated as not set by the user. Jobs submitted through the HTTP API have
// their resources canonicalized to Nomad's built-in defaults before they
// reach the server, so a value equal to the built-in default is treated as
// unset as well.
func quotaTaskResourceUnset(value, builtinDefault int) bool {
	return value == 0 || value == builtinDefault
}

// applyQuotaTaskDefaults fills in the unset resources of a task.
func applyQuotaTaskDefaults(task *structs.Task, defaults *structs.Resources) {
	if task == nil || defaults == nil {
		return
	}
	if task.Resources == nil {
		task.Resources = structs.DefaultResources()
	}
	builtin := structs.DefaultResources()
	r := task.Resources

	// CPU is only defaulted if the task doesn't reserve cores instead.
	if defaults.CPU > 0 && r.Cores == 0 && quotaTaskResourceUnset(r.CPU, builtin.CPU) {
		r.CPU = defaults.CPU
	}
	if defaults.MemoryMB > 0 && quotaTaskResourceUnset(r.MemoryMB, builtin.MemoryMB) {
		r.MemoryMB = defaults.MemoryMB
	}
	if defaults.MemoryMaxMB > 0 && r.MemoryMaxMB == 0 && defaults.MemoryMaxMB >= r.MemoryMB {
		r.MemoryMaxMB = defaults.MemoryMaxMB
	}
}

// jobQuotaValidatingHook rejects jobs whose tasks exceed the task_max of the
// namespace's quota, and with enforce = "submit" jobs whose total demand
// would by itself exceed the quota limit in this region.
type jobQuotaValidatingHook struct {
	srv *Server
}

func (jobQuotaValidatingHook) Name() string {
	return "quota-validation"
}

func (h jobQuotaValidatingHook) Validate(job *structs.Job) ([]error, error) {
	spec, err := jobQuotaSpec(h.srv, job)
	if err != nil {
		return nil, err
	}
	if spec == nil {
		return nil, nil
	}

	var mErr *multierror.Error
	if spec.TaskMax != nil {
		for _, tg := range job.TaskGroups {
			for _, task := range tg.Tasks {
				if err := validateQuotaTaskMax(task, spec.TaskMax); err != nil {
					mErr = multierror.Append(mErr, fmt.Errorf(
						"task %q in group %q exceeds the task_max of quota %q: %v", task.Name, tg.Name, spec.Name, err))
				}
			}
		}
	}

	if spec.Enforce == structs.QuotaEnforceSubmit {
		if limit := spec.LimitForRegion(h.srv.Region()); limit != nil {
			demand := structs.JobQuotaDemand(job)
			if dims := limit.CheckAdditional(structs.QuotaResources{}, demand); len(dims) > 0 {
				mErr = multierror.Append(mErr, fmt.Errorf(
					"job demand exceeds quota %q: %s", spec.Name, strings.Join(dims, ", ")))
			}
		}
	}

	return nil, mErr.ErrorOrNil()
}

// validateQuotaTaskMax returns an error if the task's resources exceed the
// maximum. A zero maximum means no maximum.
func validateQuotaTaskMax(task *structs.Task, maxRes *structs.Resources) error {
	if task == nil || task.Resources == nil {
		return nil
	}
	r := task.Resources
	var mErr *multierror.Error
	check := func(name string, value, limit int) {
		if limit > 0 && value > limit {
			mErr = multierror.Append(mErr, fmt.Errorf("%s %d > %d", name, value, limit))
		}
	}
	check(structs.QuotaDimensionCPU, r.CPU, maxRes.CPU)
	check("cores", r.Cores, maxRes.Cores)
	check(structs.QuotaDimensionMemory, r.MemoryMB, maxRes.MemoryMB)
	check(structs.QuotaDimensionMemoryMax, r.MemoryMaxMB, maxRes.MemoryMaxMB)
	return mErr.ErrorOrNil()
}

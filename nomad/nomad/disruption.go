// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/armon/go-metrics"
	"github.com/hashicorp/go-memdb"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// The disruption budget computation shared by the drainer, the eviction API
// and the status endpoint is structs.NewDisruptionStatus; it lives in the
// structs package so that the drainer and the state store can use it.

// Evict stops and reschedules an allocation like a migration if the
// disruption budget of its task group allows it. The budget is checked when
// the eviction is applied by the FSM, in the same transaction as the write,
// so concurrent evictions cannot together exceed it. If the budget does not
// allow the eviction, Evict returns a coded 429 error.
func (a *Alloc) Evict(args *structs.AllocEvictRequest, reply *structs.AllocEvictResponse) error {
	authErr := a.srv.Authenticate(a.ctx, args)
	if done, err := a.srv.forward("Alloc.Evict", args, args, reply); done {
		return err
	}
	a.srv.MeasureRPCRate("alloc", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	defer metrics.MeasureSince([]string{"nomad", "alloc", "evict"}, time.Now())

	alloc, err := getAlloc(a.srv.State(), args.AllocID)
	if err != nil {
		return err
	}

	// Evicting is stopping: check for namespace alloc-lifecycle permissions.
	allowNsOp := acl.NamespaceValidator(acl.NamespaceCapabilityAllocLifecycle)
	aclObj, err := a.srv.ResolveACL(args)
	if err != nil {
		return err
	} else if !allowNsOp(aclObj, alloc.Namespace) {
		return structs.ErrPermissionDenied
	}

	// Servers that do not know the message type cannot apply it.
	if !ServersMeetMinimumNorcaVersion(a.srv.Members(), a.srv.Region(), false) {
		return structs.NewErrRPCCodedf(http.StatusBadRequest,
			"eviction requires all servers to run Norca %s or later", minNorcaFeaturesVersion)
	}

	// Nothing to do if the allocation is already stopping or migrating.
	if alloc.TerminalStatus() || alloc.DesiredTransition.ShouldMigrate() {
		reply.Index = alloc.ModifyIndex
		return nil
	}

	now := time.Now().UTC().UnixNano()
	req := &structs.AllocEvictRequest{
		AllocID:         args.AllocID,
		NoShutdownDelay: args.NoShutdownDelay,
		Eval: &structs.Evaluation{
			ID:             uuid.Generate(),
			Namespace:      alloc.Namespace,
			Priority:       alloc.Job.Priority,
			Type:           alloc.Job.Type,
			TriggeredBy:    structs.EvalTriggerAllocStop,
			JobID:          alloc.Job.ID,
			JobModifyIndex: alloc.Job.ModifyIndex,
			Status:         structs.EvalStatusPending,
			CreateTime:     now,
			ModifyTime:     now,
		},
	}

	// raftApply returns the FSM response error as its error.
	resp, index, err := a.srv.raftApply(structs.AllocEvictRequestType, req)
	if errors.Is(err, structs.ErrDisruptionBudgetExceeded) {
		return structs.NewErrRPCCoded(http.StatusTooManyRequests, err.Error())
	} else if err != nil {
		a.logger.Error("AllocEvictRequest failed", "error", err)
		return err
	}

	reply.Index = index
	if evicted, _ := resp.(bool); evicted {
		reply.EvalID = req.Eval.ID
	}
	return nil
}

// Disruption returns the disruption budget status of each task group of a
// job that has a disruption block.
func (j *Job) Disruption(args *structs.JobDisruptionRequest, reply *structs.JobDisruptionResponse) error {
	authErr := j.srv.Authenticate(j.ctx, args)
	if done, err := j.srv.forward("Job.Disruption", args, args, reply); done {
		return err
	}
	j.srv.MeasureRPCRate("job", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "job", "disruption"}, time.Now())

	if aclObj, err := j.srv.ResolveACL(args); err != nil {
		return err
	} else if !aclObj.AllowNsOp(args.RequestNamespace(), acl.NamespaceCapabilityReadJob) {
		return structs.ErrPermissionDenied
	}

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, state *state.StateStore) error {
			out, index, err := state.DisruptionStatusByJob(ws, args.RequestNamespace(), args.JobID)
			if err != nil {
				return err
			}
			reply.TaskGroups = out
			reply.JobFound = out != nil
			reply.Index = index
			j.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return j.srv.blockingRPC(&opts)
}

// jobDisruptionHook validates the disruption blocks of a job against the
// cluster: the servers must support them, and budgets that block every
// voluntary disruption produce warnings.
type jobDisruptionHook struct {
	srv *Server
}

func (jobDisruptionHook) Name() string {
	return "disruption"
}

func (h jobDisruptionHook) Validate(job *structs.Job) ([]error, error) {
	if !job.UsesDisruptionBudgets() {
		return nil, nil
	}

	// Servers that do not know the block would silently drop it.
	if !ServersMeetMinimumNorcaVersion(h.srv.Members(), h.srv.Region(), false) {
		return nil, fmt.Errorf("disruption blocks require all servers to run Norca %s or later",
			minNorcaFeaturesVersion)
	}

	var warnings []error
	for _, tg := range job.TaskGroups {
		if tg.Disruption == nil || tg.Disruption.Validate() != nil {
			continue
		}
		if tg.Count > 0 && tg.Disruption.RequiredHealthy(tg.Count) >= tg.Count {
			warnings = append(warnings, fmt.Errorf(
				"group %q: disruption budget (%s) blocks every voluntary disruption, node drains will wait for their deadline",
				tg.Name, tg.Disruption))
		}
	}
	return warnings, nil
}

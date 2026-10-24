// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/hashicorp/go-multierror"
)

// DisruptionRetryAfterSeconds is the delay the eviction API suggests to the
// caller (Retry-After header) when the disruption budget does not allow an
// eviction.
const DisruptionRetryAfterSeconds = 5

// TypeAllocationEvicted is the event type of the allocation and evaluation
// events of an eviction (topics Allocation and Evaluation).
const TypeAllocationEvicted = "AllocationEvicted"

// ErrDisruptionBudgetExceeded is returned when a voluntary disruption, such
// as an eviction, is not allowed by the disruption budget of the task group.
var ErrDisruptionBudgetExceeded = errors.New("disruption budget exceeded")

// DisruptionBudget limits the voluntary disruptions (node drain, node pool
// scale-down, eviction) of a task group. Exactly one of MinHealthy and
// MaxUnavailable is set; both take either an absolute count ("4") or a
// percentage of the group count ("25%"). Percentages are rounded up, as
// Kubernetes does for PodDisruptionBudgets.
//
// The budget does not apply to reducing the group count, to deployments
// (those are governed by the update block) or to involuntary disruptions.
type DisruptionBudget struct {
	// MinHealthy is the number of allocations that must remain healthy.
	MinHealthy string

	// MaxUnavailable is the number of allocations that may be unavailable
	// at the same time.
	MaxUnavailable string
}

func (d *DisruptionBudget) Copy() *DisruptionBudget {
	if d == nil {
		return nil
	}
	nd := *d
	return &nd
}

// Canonicalize trims surrounding whitespace from the values.
func (d *DisruptionBudget) Canonicalize() {
	if d == nil {
		return
	}
	d.MinHealthy = strings.TrimSpace(d.MinHealthy)
	d.MaxUnavailable = strings.TrimSpace(d.MaxUnavailable)
}

func (d *DisruptionBudget) Validate() error {
	var mErr multierror.Error
	switch {
	case d.MinHealthy == "" && d.MaxUnavailable == "":
		_ = multierror.Append(&mErr, errors.New("disruption block must set min_healthy or max_unavailable"))
	case d.MinHealthy != "" && d.MaxUnavailable != "":
		_ = multierror.Append(&mErr, errors.New("disruption block cannot set both min_healthy and max_unavailable"))
	}
	if d.MinHealthy != "" {
		if _, _, err := parseDisruptionValue(d.MinHealthy); err != nil {
			_ = multierror.Append(&mErr, fmt.Errorf("invalid min_healthy: %v", err))
		}
	}
	if d.MaxUnavailable != "" {
		if _, _, err := parseDisruptionValue(d.MaxUnavailable); err != nil {
			_ = multierror.Append(&mErr, fmt.Errorf("invalid max_unavailable: %v", err))
		}
	}
	return mErr.ErrorOrNil()
}

// RequiredHealthy returns the number of allocations of a group with the given
// count that must remain healthy under the budget. Invalid budgets require
// every allocation to be healthy.
func (d *DisruptionBudget) RequiredHealthy(count int) int {
	if d.MinHealthy != "" {
		v, pct, err := parseDisruptionValue(d.MinHealthy)
		if err != nil {
			return count
		}
		if pct {
			return percentOfCeil(count, v)
		}
		return v
	}

	v, pct, err := parseDisruptionValue(d.MaxUnavailable)
	if err != nil {
		return count
	}
	if pct {
		v = percentOfCeil(count, v)
	}
	return max(0, count-v)
}

// String returns the budget in jobspec form, e.g. "min_healthy = 4".
func (d *DisruptionBudget) String() string {
	if d == nil {
		return ""
	}
	if d.MinHealthy != "" {
		return "min_healthy = " + d.MinHealthy
	}
	return "max_unavailable = " + d.MaxUnavailable
}

// parseDisruptionValue parses a non-negative count or a percentage between
// 0% and 100%.
func parseDisruptionValue(s string) (int, bool, error) {
	pct := strings.HasSuffix(s, "%")
	n, err := strconv.Atoi(strings.TrimSuffix(s, "%"))
	if err != nil {
		return 0, false, fmt.Errorf("%q is not a count or a percentage", s)
	}
	if n < 0 {
		return 0, false, fmt.Errorf("%q must not be negative", s)
	}
	if pct && n > 100 {
		return 0, false, fmt.Errorf("%q must not exceed 100%%", s)
	}
	return n, pct, nil
}

func percentOfCeil(count, pct int) int {
	return (count*pct + 99) / 100
}

// validateDisruption validates the disruption block of a task group.
func (tg *TaskGroup) validateDisruption(j *Job) error {
	if tg.Disruption == nil {
		return nil
	}
	if j.Type != JobTypeService {
		return fmt.Errorf("Job type %q does not allow disruption block", j.Type)
	}
	return tg.Disruption.Validate()
}

// UsesDisruptionBudgets returns whether any task group of the job has a
// disruption block.
func (j *Job) UsesDisruptionBudgets() bool {
	for _, tg := range j.TaskGroups {
		if tg.Disruption != nil {
			return true
		}
	}
	return false
}

// HealthyForDisruption returns whether the allocation counts as healthy for
// disruption budgets: it is running, is marked healthy by the deployment or
// migration health check, and is not already marked for migration (a
// pending disruption).
func (a *Allocation) HealthyForDisruption() bool {
	return !a.TerminalStatus() &&
		a.ClientStatus == AllocClientStatusRunning &&
		a.DeploymentStatus.IsHealthy() &&
		!a.DesiredTransition.ShouldMigrate()
}

// DisruptionStatus is the current state of the disruption budget of a task
// group.
type DisruptionStatus struct {
	// TaskGroup is the name of the task group.
	TaskGroup string

	// MinHealthy and MaxUnavailable are copied from the budget.
	MinHealthy     string
	MaxUnavailable string

	// DesiredCount is the count of the task group the budget is relative to.
	DesiredCount int

	// RequiredHealthy is the number of allocations that must stay healthy.
	RequiredHealthy int

	// Healthy is the number of allocations currently counting as healthy.
	Healthy int

	// DeploymentReserved is the number of disruptions held back for the next
	// step of an active deployment of the job.
	DeploymentReserved int

	// DisruptionsAllowed is the number of healthy allocations that may be
	// disrupted now.
	DisruptionsAllowed int
}

// NewDisruptionStatus computes the disruption budget status of a task group
// from the allocations of its job and the latest deployment of the job
// (which may be nil). It returns nil if the group has no disruption block.
//
// This is the single computation used by the drainer, the eviction API and
// the status endpoint.
//
// While a deployment of the job is running and still has allocations of the
// group to place, the budget keeps room for the deployment's next step:
// update.max_parallel minus the allocations the deployment already has in
// flight (placed but without health). Deployments are not limited by the
// budget, so this prevents a drain or eviction from combining with the next
// deployment step to go below the budget.
func NewDisruptionStatus(tg *TaskGroup, allocs []*Allocation, d *Deployment) *DisruptionStatus {
	if tg == nil || tg.Disruption == nil {
		return nil
	}

	s := &DisruptionStatus{
		TaskGroup:       tg.Name,
		MinHealthy:      tg.Disruption.MinHealthy,
		MaxUnavailable:  tg.Disruption.MaxUnavailable,
		DesiredCount:    tg.Count,
		RequiredHealthy: tg.Disruption.RequiredHealthy(tg.Count),
	}

	for _, alloc := range allocs {
		if alloc.TaskGroup == tg.Name && alloc.HealthyForDisruption() {
			s.Healthy++
		}
	}

	s.DeploymentReserved = deploymentDisruptionReserve(tg, d)
	s.DisruptionsAllowed = max(0, s.Healthy-s.RequiredHealthy-s.DeploymentReserved)
	return s
}

// deploymentDisruptionReserve returns the number of disruptions held back for
// the next step of an active deployment.
func deploymentDisruptionReserve(tg *TaskGroup, d *Deployment) int {
	if d == nil || tg.Update.IsEmpty() {
		return 0
	}
	switch d.Status {
	case DeploymentStatusRunning, DeploymentStatusPending,
		DeploymentStatusInitializing, DeploymentStatusUnblocking:
	default:
		// Paused, blocked and terminal deployments make no further steps
		// on their own.
		return 0
	}
	ds, ok := d.TaskGroups[tg.Name]
	if !ok {
		return 0
	}
	remaining := ds.DesiredTotal - ds.PlacedAllocs
	if remaining <= 0 {
		return 0
	}
	inFlight := max(0, ds.PlacedAllocs-ds.HealthyAllocs-ds.UnhealthyAllocs)
	return min(remaining, max(0, tg.Update.MaxParallel-inFlight))
}

// Disruptable returns the subset of the candidate allocations that may be
// disrupted now. Allocations not counting as healthy can be disrupted as long
// as the group has at least the required number of healthy allocations;
// healthy allocations only up to DisruptionsAllowed. Unhealthy candidates are
// returned first. Candidates that are already terminal on the server are
// never returned.
func (s *DisruptionStatus) Disruptable(candidates []*Allocation) []*Allocation {
	if s.Healthy < s.RequiredHealthy {
		return nil
	}

	var unhealthy, healthy []*Allocation
	for _, alloc := range candidates {
		if alloc.TerminalStatus() {
			// Already being stopped: not a new disruption.
			continue
		}
		if alloc.HealthyForDisruption() {
			if len(healthy) < s.DisruptionsAllowed {
				healthy = append(healthy, alloc)
			}
		} else {
			unhealthy = append(unhealthy, alloc)
		}
	}
	return append(unhealthy, healthy...)
}

// Error returns an error wrapping ErrDisruptionBudgetExceeded that describes
// why the status does not allow a disruption.
func (s *DisruptionStatus) Error() error {
	if s.DeploymentReserved > 0 {
		return fmt.Errorf("%w: task group %q has %d healthy allocations, requires %d and reserves %d for an active deployment",
			ErrDisruptionBudgetExceeded, s.TaskGroup, s.Healthy, s.RequiredHealthy, s.DeploymentReserved)
	}
	return fmt.Errorf("%w: task group %q has %d healthy allocations and requires %d",
		ErrDisruptionBudgetExceeded, s.TaskGroup, s.Healthy, s.RequiredHealthy)
}

// AllocEvictRequest is used to evict an allocation, i.e. to stop and
// reschedule it like a migration, if the disruption budget of its task group
// allows it. The budget is checked when the request is applied to the state
// store, so concurrent evictions cannot together exceed it.
type AllocEvictRequest struct {
	AllocID         string
	NoShutdownDelay bool

	// Eval is the evaluation created for the eviction. It is set by the
	// server before the request is applied.
	Eval *Evaluation

	WriteRequest
}

// AllocEvictResponse is the response to an AllocEvictRequest.
type AllocEvictResponse struct {
	// EvalID is the ID of the follow-up evaluation for the rescheduled
	// allocation. It is empty if the allocation was already being migrated.
	EvalID string

	WriteMeta
}

// JobDisruptionRequest is used to get the disruption budget status of a job.
type JobDisruptionRequest struct {
	JobID string
	QueryOptions
}

// JobDisruptionResponse returns the disruption budget status of each task
// group of a job that has a disruption block.
type JobDisruptionResponse struct {
	TaskGroups map[string]*DisruptionStatus

	// JobFound is false if the job does not exist.
	JobFound bool

	QueryMeta
}

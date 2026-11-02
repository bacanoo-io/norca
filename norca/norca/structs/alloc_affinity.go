// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/go-multierror"
)

const (
	// AllocAffinityKindColocate is the kind of a rule that places a task
	// group close to the allocations matched by its selector.
	AllocAffinityKindColocate = "colocate"

	// AllocAffinityKindSeparate is the kind of a rule that places a task
	// group away from the allocations matched by its selector.
	AllocAffinityKindSeparate = "separate"

	// AllocAffinityDefaultScope is the topology domain used when a rule
	// does not set one: the node itself.
	AllocAffinityDefaultScope = "${node.unique.id}"

	// MaxAllocAffinitiesPerGroup is the maximum number of colocate and
	// separate rules (combined) a single task group may define. The cost of
	// the rules is paid once per evaluation and task group, so the limit
	// keeps that cost bounded.
	MaxAllocAffinitiesPerGroup = 8
)

// AllocAffinity is a cross-job placement rule of a task group. It relates
// the placement of the group to the allocations of other jobs (and of its own
// job) selected by the meta fields of their job and task group.
//
// A colocate rule prefers (or with Required, demands) nodes whose topology
// domain already holds a matching allocation; a separate rule prefers (or
// demands) nodes whose domain holds none. The rules apply only at placement
// time: running allocations are never moved because of them, and they are
// not symmetric.
type AllocAffinity struct {
	// Selector is the set of meta key/value pairs an allocation's job and
	// task group meta (task group values override job values) must all
	// contain for the allocation to match.
	Selector map[string]string

	// Namespace is the namespace in which matching allocations are searched.
	// Empty means the namespace of the job defining the rule.
	Namespace string

	// Scope is the node attribute interpolation that defines the topology
	// domain, for example ${node.unique.id}, ${node.datacenter} or
	// ${meta.rack}.
	Scope string

	// Weight is the preference weight in the range [-100, 100]. It is only
	// used when Required is false, and must be zero when Required is true.
	Weight int8

	// Required turns the rule into a hard feasibility filter.
	Required bool
}

// Copy returns a deep copy of the rule.
func (a *AllocAffinity) Copy() *AllocAffinity {
	if a == nil {
		return nil
	}
	na := *a
	na.Selector = maps.Clone(a.Selector)
	return &na
}

// Equal returns whether two rules are identical.
func (a *AllocAffinity) Equal(o *AllocAffinity) bool {
	if a == nil || o == nil {
		return a == o
	}
	return a.Namespace == o.Namespace &&
		a.Scope == o.Scope &&
		a.Weight == o.Weight &&
		a.Required == o.Required &&
		maps.Equal(a.Selector, o.Selector)
}

// String returns a short, stable, human readable form of the rule, used in
// placement metrics and log lines.
func (a *AllocAffinity) String() string {
	keys := make([]string, 0, len(a.Selector))
	for k := range a.Selector {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, len(keys))
	for i, k := range keys {
		pairs[i] = k + "=" + a.Selector[k]
	}
	sel := strings.Join(pairs, ",")
	if a.Namespace != "" {
		sel = a.Namespace + "/" + sel
	}
	return fmt.Sprintf("{%s} by %s", sel, a.Scope)
}

// Canonicalize sets the defaults of the rule.
func (a *AllocAffinity) Canonicalize() {
	if a.Scope == "" {
		a.Scope = AllocAffinityDefaultScope
	}
	if len(a.Selector) == 0 {
		a.Selector = nil
	}
}

// Validate validates a rule of the given kind (colocate or separate) defined
// in a job of the given type.
func (a *AllocAffinity) Validate(kind, jobType string) error {
	var mErr multierror.Error

	if len(a.Selector) == 0 {
		mErr.Errors = append(mErr.Errors, errors.New("Missing selector"))
	}
	for k := range a.Selector {
		if k == "" {
			mErr.Errors = append(mErr.Errors, errors.New("Selector keys must not be empty"))
			break
		}
	}

	if a.Namespace != "" && !validNamespaceName.MatchString(a.Namespace) {
		mErr.Errors = append(mErr.Errors, fmt.Errorf("Invalid namespace %q", a.Namespace))
	}

	if !ValidAllocAffinityScope(a.Scope) {
		mErr.Errors = append(mErr.Errors, fmt.Errorf(
			"Invalid scope %q: must be a node attribute interpolation such as ${node.unique.id}, ${node.datacenter}, ${attr.<name>} or ${meta.<name>}",
			a.Scope))
	}

	if a.Required {
		if a.Weight != 0 {
			mErr.Errors = append(mErr.Errors, errors.New("Weight must not be set on a required rule"))
		}
	} else {
		switch jobType {
		case JobTypeSystem, JobTypeSysBatch:
			mErr.Errors = append(mErr.Errors, fmt.Errorf("%s jobs may only have required %s rules", jobType, kind))
		}
		if a.Weight == 0 || a.Weight < -100 || a.Weight > 100 {
			mErr.Errors = append(mErr.Errors, errors.New("Weight must be a non-zero value between -100 and 100"))
		}
	}

	return mErr.ErrorOrNil()
}

// ValidAllocAffinityScope returns whether the scope is a node attribute
// interpolation the scheduler can resolve.
func ValidAllocAffinityScope(scope string) bool {
	switch scope {
	case "${node.unique.id}", "${node.datacenter}", "${node.unique.name}",
		"${node.class}", "${node.pool}":
		return true
	}
	for _, prefix := range []string{"${attr.", "${meta."} {
		if strings.HasPrefix(scope, prefix) && strings.HasSuffix(scope, "}") &&
			len(scope) > len(prefix)+1 {
			return true
		}
	}
	return false
}

// TargetNamespace returns the namespace in which the rule looks for matching
// allocations when defined in a job of the given namespace.
func (a *AllocAffinity) TargetNamespace(jobNamespace string) string {
	if a.Namespace != "" {
		return a.Namespace
	}
	return jobNamespace
}

// Matches returns whether an allocation of a task group with the given job
// and task group meta is selected by the rule. Task group meta overrides job
// meta for the same key.
func (a *AllocAffinity) Matches(jobMeta, groupMeta map[string]string) bool {
	if len(a.Selector) == 0 {
		return false
	}
	for k, want := range a.Selector {
		v, ok := groupMeta[k]
		if !ok {
			v, ok = jobMeta[k]
		}
		if !ok || v != want {
			return false
		}
	}
	return true
}

// CopySliceAllocAffinities returns a deep copy of the rules.
func CopySliceAllocAffinities(s []*AllocAffinity) []*AllocAffinity {
	if s == nil {
		return nil
	}
	out := make([]*AllocAffinity, len(s))
	for i, a := range s {
		out[i] = a.Copy()
	}
	return out
}

// HasAllocAffinities returns whether the task group defines any colocate or
// separate rule.
func (tg *TaskGroup) HasAllocAffinities() bool {
	return len(tg.Colocate) > 0 || len(tg.Separate) > 0
}

// canonicalizeAllocAffinities sets the defaults of the colocate and separate
// rules of the task group.
func (tg *TaskGroup) canonicalizeAllocAffinities() {
	for _, a := range tg.Colocate {
		a.Canonicalize()
	}
	for _, a := range tg.Separate {
		a.Canonicalize()
	}
}

// validateAllocAffinities validates the colocate and separate rules of the
// task group.
func (tg *TaskGroup) validateAllocAffinities(j *Job) error {
	var mErr multierror.Error
	if n := len(tg.Colocate) + len(tg.Separate); n > MaxAllocAffinitiesPerGroup {
		mErr.Errors = append(mErr.Errors, fmt.Errorf(
			"Task group may define at most %d colocate and separate rules, got %d",
			MaxAllocAffinitiesPerGroup, n))
	}
	for idx, a := range tg.Colocate {
		if err := a.Validate(AllocAffinityKindColocate, j.Type); err != nil {
			mErr.Errors = append(mErr.Errors, fmt.Errorf("Colocate %d validation failed: %s", idx+1, err))
		}
	}
	for idx, a := range tg.Separate {
		if err := a.Validate(AllocAffinityKindSeparate, j.Type); err != nil {
			mErr.Errors = append(mErr.Errors, fmt.Errorf("Separate %d validation failed: %s", idx+1, err))
		}
	}
	return mErr.ErrorOrNil()
}

// UsesAllocAffinities returns whether any task group of the job defines a
// colocate or separate rule.
func (j *Job) UsesAllocAffinities() bool {
	for _, tg := range j.TaskGroups {
		if tg.HasAllocAffinities() {
			return true
		}
	}
	return false
}

// AllocAffinityNamespaces returns the sorted, distinct namespaces other than
// the job's own namespace in which its colocate and separate rules look for
// allocations.
func (j *Job) AllocAffinityNamespaces() []string {
	var out []string
	for _, tg := range j.TaskGroups {
		for _, rules := range [][]*AllocAffinity{tg.Colocate, tg.Separate} {
			for _, a := range rules {
				ns := a.TargetNamespace(j.Namespace)
				if ns != j.Namespace && !slices.Contains(out, ns) {
					out = append(out, ns)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/nomad/structs"
)

// allocAffinityManyJobs is the number of jobs selected by a single colocate
// or separate rule above which registration returns a warning, because the
// scheduling cost of the rule grows with the number of matching allocations.
const allocAffinityManyJobs = 1000

// jobAllocAffinityHook validates the colocate and separate (cross-job
// affinity) rules of a job against the cluster: the servers must support
// them, and rules that refer to missing namespaces or select a very large
// number of jobs produce warnings.
type jobAllocAffinityHook struct {
	srv *Server
}

func (jobAllocAffinityHook) Name() string {
	return "alloc-affinity"
}

func (h jobAllocAffinityHook) Validate(job *structs.Job) ([]error, error) {
	if !job.UsesAllocAffinities() {
		return nil, nil
	}

	// Servers that do not know the rules would silently drop them.
	if !ServersMeetMinimumVersion(h.srv.Members(), h.srv.Region(), minNorcaFeaturesVersion, false) {
		return nil, fmt.Errorf("colocate and separate blocks require all servers to run version %s or later",
			minNorcaFeaturesVersion)
	}

	state := h.srv.State()
	var warnings []error

	for _, ns := range job.AllocAffinityNamespaces() {
		existing, err := state.NamespaceByName(nil, ns)
		if err != nil {
			return nil, err
		}
		if existing == nil {
			warnings = append(warnings, fmt.Errorf(
				"colocate/separate rule refers to nonexistent namespace %q", ns))
		}
	}

	for _, tg := range job.TaskGroups {
		for _, rules := range [][]*structs.AllocAffinity{tg.Colocate, tg.Separate} {
			for _, rule := range rules {
				n, err := h.countSelectedJobs(job, rule)
				if err != nil {
					return nil, err
				}
				if n > allocAffinityManyJobs {
					warnings = append(warnings, fmt.Errorf(
						"Task group %q: rule %s selects %d jobs; scheduling cost grows with the number of matching allocations",
						tg.Name, rule, n))
				}
			}
		}
	}

	return warnings, nil
}

// countSelectedJobs returns the number of jobs in the rule's target namespace
// with at least one task group selected by the rule. It stops counting above
// the warning threshold.
func (h jobAllocAffinityHook) countSelectedJobs(job *structs.Job, rule *structs.AllocAffinity) (int, error) {
	iter, err := h.srv.State().JobsByNamespace(nil, rule.TargetNamespace(job.Namespace))
	if err != nil {
		return 0, err
	}
	n := 0
	for raw := iter.Next(); raw != nil && n <= allocAffinityManyJobs; raw = iter.Next() {
		other := raw.(*structs.Job)
		for _, tg := range other.TaskGroups {
			if rule.Matches(other.Meta, tg.Meta) {
				n++
				break
			}
		}
	}
	return n, nil
}

// allowAllocAffinityNamespaces returns whether the ACL object may read the
// jobs of the namespaces, other than the job's own, that the colocate and
// separate rules of the job observe.
func allowAllocAffinityNamespaces(aclObj *acl.ACL, job *structs.Job) bool {
	for _, ns := range job.AllocAffinityNamespaces() {
		if !aclObj.AllowNsOp(ns, acl.NamespaceCapabilityReadJob) {
			return false
		}
	}
	return true
}

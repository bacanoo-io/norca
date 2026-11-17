// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

// TestManagedByAllows_WorkloadJobIDWithSlash checks that the workload
// identity of a job whose ID continues the ID of another job with a slash
// ("ctrl/evil") is not taken for an identity of that job ("ctrl").
func TestManagedByAllows_WorkloadJobIDWithSlash(t *testing.T) {
	ci.Parallel(t)

	workload := func(ns, job, task string) *AdmissionUser {
		return NewAdmissionUser(nil, &IdentityClaims{Namespace: ns, JobID: job, TaskName: task})
	}
	allows := func(managedBy string, u *AdmissionUser) bool {
		enforced, allowed := ManagedByAllows(managedBy, u)
		must.True(t, enforced)
		return allowed
	}

	jobLevel := ManagedByWorkloadPrefix + "default/ctrl"
	taskLevel := ManagedByWorkloadPrefix + "default/ctrl/main"

	// The identity the value names.
	must.True(t, allows(jobLevel, workload("default", "ctrl", "main")))
	must.True(t, allows(jobLevel, workload("default", "ctrl", "sidecar")))
	must.True(t, allows(taskLevel, workload("default", "ctrl", "main")))
	must.False(t, allows(taskLevel, workload("default", "ctrl", "sidecar")))
	must.False(t, allows(jobLevel, workload("other", "ctrl", "main")))
	must.False(t, allows(jobLevel, workload("default", "ctrl2", "main")))

	// Jobs anyone with submit-job in the namespace can register.
	must.False(t, allows(jobLevel, workload("default", "ctrl/evil", "t")),
		must.Sprint("job ctrl/evil passes for job ctrl"))
	must.False(t, allows(taskLevel, workload("default", "ctrl/main", "t")),
		must.Sprint("job ctrl/main passes for task main of job ctrl"))
	must.False(t, allows(taskLevel, workload("default", "ctrl/main/x", "t")))

	// A job with a slash (or the escape character) in its ID is named by
	// its escaped ID, and only that job matches.
	must.Eq(t, "workload:default/a%2Fb%25c/t", workload("default", "a/b%c", "t").Name)
	slashed := ManagedByWorkload("default", "team/ctrl")
	must.Eq(t, "workload:default/team%2Fctrl", slashed)
	must.True(t, allows(slashed, workload("default", "team/ctrl", "main")))
	must.False(t, allows(slashed, workload("default", "team", "ctrl")))
	must.False(t, allows(slashed, workload("default", "team%2Fctrl", "main")))
	must.False(t, allows(slashed, workload("default", "team/ctrl/evil", "main")))
}

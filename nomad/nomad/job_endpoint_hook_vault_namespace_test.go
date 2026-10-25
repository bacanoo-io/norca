// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

func TestJobVaultNamespaceDefaulter(t *testing.T) {
	ci.Parallel(t)

	job := mock.Job()
	job.VaultNamespace = "job-ns"
	tg := job.TaskGroups[0]
	unset := tg.Tasks[0]
	unset.Vault = &structs.Vault{Policies: []string{"a"}}
	own := unset.Copy()
	own.Name = "own"
	own.Vault = &structs.Vault{Policies: []string{"b"}, Namespace: "task-ns"}
	noVault := unset.Copy()
	noVault.Name = "no-vault"
	noVault.Vault = nil
	tg.Tasks = []*structs.Task{unset, own, noVault}

	out, warnings, err := jobVaultNamespaceDefaulter{}.Mutate(job)
	must.NoError(t, err)
	must.Nil(t, warnings)
	must.Eq(t, "job-ns", out.TaskGroups[0].Tasks[0].Vault.Namespace)
	must.Eq(t, "task-ns", out.TaskGroups[0].Tasks[1].Vault.Namespace)
	must.Nil(t, out.TaskGroups[0].Tasks[2].Vault)

	// Without a job-level namespace nothing changes
	job2 := mock.Job()
	job2.TaskGroups[0].Tasks[0].Vault = &structs.Vault{Policies: []string{"a"}}
	out2, _, err := jobVaultNamespaceDefaulter{}.Mutate(job2)
	must.NoError(t, err)
	must.Eq(t, "", out2.TaskGroups[0].Tasks[0].Vault.Namespace)
}

func TestJobEndpoint_Register_VaultNamespaceDefault(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, nil)
	defer cleanupS1()
	testutil.WaitForLeader(t, s1.RPC)

	job := mock.Job()
	job.VaultNamespace = "job-ns"
	job.TaskGroups[0].Tasks[0].Vault = &structs.Vault{Policies: []string{"default"}}

	// The mutator runs before the Vault validator, so the defaulted
	// namespace is what the rest of the chain sees
	out, _, err := NewJobEndpoints(s1, nil).admissionMutators(job, nil)
	must.NoError(t, err)
	must.Eq(t, "job-ns", out.TaskGroups[0].Tasks[0].Vault.Namespace)
}

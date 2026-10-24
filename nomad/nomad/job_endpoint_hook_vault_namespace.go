// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"github.com/hashicorp/nomad/nomad/structs"
)

// jobVaultNamespaceDefaulter makes the job-level Vault namespace the default
// for every task Vault block that does not set its own namespace. The HTTP
// agent already did this when converting API jobs; doing it on the server as
// well makes jobs submitted directly over RPC behave the same way.
type jobVaultNamespaceDefaulter struct{}

func (jobVaultNamespaceDefaulter) Name() string {
	return "vault-namespace-default"
}

func (jobVaultNamespaceDefaulter) Mutate(job *structs.Job) (*structs.Job, []error, error) {
	if job.VaultNamespace == "" {
		return job, nil, nil
	}
	for _, tg := range job.TaskGroups {
		for _, task := range tg.Tasks {
			if task.Vault != nil && task.Vault.Namespace == "" {
				task.Vault.Namespace = job.VaultNamespace
			}
		}
	}
	return job, nil, nil
}

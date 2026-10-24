// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package wrapper

import (
	"github.com/hashicorp/nomad/client/serviceregistration"
	"github.com/hashicorp/nomad/client/serviceregistration/checks/checkstore"
	"github.com/hashicorp/nomad/nomad/structs"
)

// SetCheckStore sets the store of the results of the checks executed by the
// client. When set, the results of the role checks of a removed workload are
// reset to pending, so a restarted task has to pass its startup and readiness
// checks again instead of inheriting the results of its previous run.
//
// Must be called before the wrapper is used.
func (h *HandlerWrapper) SetCheckStore(shim checkstore.Shim) {
	h.checkStore = shim
}

// resetRoleChecks resets the results of the client executed role checks of
// workload to pending. Checks without a role are left untouched.
func (h *HandlerWrapper) resetRoleChecks(workload *serviceregistration.WorkloadServices) {
	if h.checkStore == nil {
		return
	}

	var ids []structs.CheckID
	for _, service := range workload.Services {
		for _, check := range service.Checks {
			if check.Role != "" && check.IsClientExecuted(service.Provider) {
				ids = append(ids, structs.NomadCheckID(workload.AllocInfo.AllocID, workload.AllocInfo.Group, check))
			}
		}
	}

	if len(ids) == 0 {
		return
	}

	if err := h.checkStore.Reset(workload.AllocInfo.AllocID, ids); err != nil {
		h.log.Warn("failed to reset check results", "alloc_id", workload.AllocInfo.AllocID, "error", err)
	}
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consul

import (
	"github.com/hashicorp/nomad/client/serviceregistration"
	"github.com/hashicorp/nomad/nomad/structs"
)

// isConsulCheck returns true if check is registered in Consul. Liveness checks
// are not: the Nomad client executes them, so their result does not affect
// the health of the service in Consul.
func isConsulCheck(check *structs.ServiceCheck) bool {
	return !check.IsClientExecuted(structs.ServiceProviderConsul)
}

// watchCheckID returns the ID under which the check watcher follows check of
// the service of serviceID: the Consul check ID, or for checks executed by the
// Nomad client, the ID of their result in the client check store.
func watchCheckID(serviceID string, workload *serviceregistration.WorkloadServices, check *structs.ServiceCheck) string {
	if !isConsulCheck(check) {
		return string(structs.NomadCheckID(workload.AllocInfo.AllocID, workload.AllocInfo.Group, check))
	}
	return MakeCheckID(serviceID, check)
}

// SetLocalCheckStatusGetter sets the getter of the statuses of the checks the
// Nomad client executes on behalf of Consul services (liveness checks), so the
// check watcher can restart their tasks.
func (c *ServiceClient) SetLocalCheckStatusGetter(getter serviceregistration.CheckStatusGetter) {
	c.checkWatcher.SetLocalGetter(getter)
}

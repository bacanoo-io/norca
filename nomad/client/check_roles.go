// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"github.com/hashicorp/nomad/client/serviceregistration"
	"github.com/hashicorp/nomad/client/serviceregistration/nsd"
)

// localCheckStatusSetter is implemented by service registration handlers whose
// check watcher can follow checks executed by the client on their behalf (the
// liveness checks of Consul services).
type localCheckStatusSetter interface {
	SetLocalCheckStatusGetter(serviceregistration.CheckStatusGetter)
}

// setupCheckRoles connects the parts of the client that health check roles
// rely on, once the check store and the service registration handlers exist:
//
//   - the service registration wrapper resets the results of the role checks
//     of a workload when it is removed, so a restarted task starts over;
//   - the Consul check watcher reads the results of the liveness checks the
//     client executes for Consul services.
func (c *Client) setupCheckRoles() {
	c.serviceRegWrapper.SetCheckStore(c.checkStore)

	if setter, ok := c.consulService.(localCheckStatusSetter); ok {
		setter.SetLocalCheckStatusGetter(nsd.NewStatusGetter(c.checkStore))
	}
}

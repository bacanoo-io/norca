// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"github.com/hashicorp/nomad/nomad/structs"
)

// BusStatus returns the status of the system bus of the server that
// receives the request, or of the server named by ServerID.
func (op *Operator) BusStatus(args *structs.BusStatusRequest, reply *structs.BusStatusResponse) error {
	// The status is local to a server: never forwarded to the leader.
	args.AllowStale = true

	authErr := op.srv.Authenticate(op.ctx, args)
	if done, err := op.srv.forward("Operator.BusStatus", args, args, reply); done {
		return err
	}
	op.srv.MeasureRPCRate("operator", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	if aclObj, err := op.srv.ResolveACL(args); err != nil {
		return err
	} else if aclObj != nil && !aclObj.AllowOperatorRead() {
		return structs.ErrPermissionDenied
	}

	if args.ServerID != "" {
		target, err := op.srv.getServer(op.srv.config.Region, args.ServerID)
		if err != nil {
			return err
		}
		if target.Name != op.srv.LocalMember().Name {
			return op.srv.forwardServer(target, "Operator.BusStatus", args, reply)
		}
	}

	status, err := op.srv.busStatus()
	if err != nil {
		return err
	}
	reply.Status = status
	return nil
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/nomad/structs"
)

// checkManagedJob protects jobs managed by a controller (operator
// framework): a job whose meta names an identity under the reserved key
// structs.JobMetaManagedBy can only be updated, stopped, reverted and scaled
// by that identity. Management tokens (including the leader's, which the
// object garbage collector uses) and clusters without ACLs are not
// restricted. Values that are not identities are not enforced.
func checkManagedJob(aclObj *acl.ACL, args structs.RequestWithIdentity, job *structs.Job) error {
	if job == nil || aclObj == nil || aclObj.IsManagement() {
		return nil
	}
	managedBy := job.Meta[structs.JobMetaManagedBy]
	if managedBy == "" {
		return nil
	}
	enforced, allowed := structs.ManagedByAllows(managedBy, admissionUser(args))
	if enforced && !allowed {
		return structs.NewObjectManagedError(fmt.Sprintf("job %q", job.ID), managedBy)
	}
	return nil
}

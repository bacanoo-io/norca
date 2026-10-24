// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

//go:build !ent
// +build !ent

package nomad

import (
	"github.com/hashicorp/nomad/nomad/structs"
)

// enforceSubmitJob is the policy slot of the submit-job scope. It runs the
// dynamic admission controllers that validate the final job: the CEL
// admission policies and the validating admission webhooks. op is
// structs.AdmissionOperationRegister or structs.AdmissionOperationPlan.
func (j *Job) enforceSubmitJob(op string, override bool, job *structs.Job, nomadACLToken *structs.ACLToken, ns *structs.Namespace) (error, error) {
	return j.enforceAdmission(op, job, nomadACLToken)
}

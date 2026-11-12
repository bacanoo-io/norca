// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build !ent
// +build !ent

package norca

import (
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// enforceSubmitJob is the policy slot of the submit-job scope. It runs the
// dynamic admission controllers that validate the final job: the CEL
// admission policies and the validating admission webhooks. op is
// structs.AdmissionOperationRegister or structs.AdmissionOperationPlan.
// override (the request's PolicyOverride) is ignored: admission policies
// cannot be overridden. user is the submitter as the mutating webhooks saw
// it (admissionUser of the request), so a rule on request.user decides the
// same in both phases and for a plan as for the registration.
func (j *Job) enforceSubmitJob(op string, override bool, job *structs.Job, user *structs.AdmissionUser, ns *structs.Namespace) (error, error) {
	return j.enforceAdmission(op, job, user)
}

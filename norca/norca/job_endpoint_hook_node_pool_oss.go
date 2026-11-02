// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build !ent
// +build !ent

package norca

import "github.com/bacanoo-io/norca/norca/norca/structs"

// enterpriseValidation implements any admission hooks for node pools for Nomad
// Enterprise.
func (j jobNodePoolValidatingHook) enterpriseValidation(_ *structs.Job, _ *structs.NodePool) ([]error, error) {
	return nil, nil
}

// jobNodePoolMutatingHook mutates the job on Nomad Enterprise only.
type jobNodePoolMutatingHook struct {
	srv *Server
}

func (c jobNodePoolMutatingHook) Name() string {
	return "node-pool-mutation"
}

func (c jobNodePoolMutatingHook) Mutate(job *structs.Job) (*structs.Job, []error, error) {
	if job.NodePool == "" {
		job.NodePool = structs.NodePoolDefault
	}

	return job, nil, nil
}

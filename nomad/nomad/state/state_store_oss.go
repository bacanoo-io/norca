// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

//go:build !ent
// +build !ent

package state

import (
	"github.com/hashicorp/nomad/nomad/structs"
)

// deleteRecommendationsByJob deletes all recommendations for the specified job
func (s *StateStore) deleteRecommendationsByJob(index uint64, txn Txn, job *structs.Job) error {
	return nil
}

// updateJobRecommendations updates/deletes job recommendations as necessary for a job update
func (s *StateStore) updateJobRecommendations(index uint64, txn Txn, prevJob, newJob *structs.Job) error {
	return nil
}

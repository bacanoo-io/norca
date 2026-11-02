// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build !ent
// +build !ent

package norca

import (
	"github.com/bacanoo-io/norca/norca/norca/state"
)

// refreshIndex returns the index the scheduler should refresh to as the maximum
// of the allocation, node and quota specification tables.
func refreshIndex(snap *state.StateSnapshot) (uint64, error) {
	allocIndex, err := snap.Index("allocs")
	if err != nil {
		return 0, err
	}
	nodeIndex, err := snap.Index("nodes")
	if err != nil {
		return 0, err
	}
	quotaIndex, err := snap.Index(state.TableQuotaSpecs)
	if err != nil {
		return 0, err
	}
	return maxUint64(nodeIndex, allocIndex, quotaIndex), nil
}

// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build !ent
// +build !ent

package raftutil

import "github.com/bacanoo-io/norca/norca/norca/state"

func insertEnterpriseState(m map[string][]interface{}, state *state.StateStore) {
}

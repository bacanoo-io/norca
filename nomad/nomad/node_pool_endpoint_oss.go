// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build !ent
// +build !ent

package nomad

import (
	"errors"

	"github.com/bacanoo-io/norca/nomad/nomad/structs"
)

func (n *NodePool) validateLicense(pool *structs.NodePool) error {
	if pool != nil && pool.SchedulerConfiguration != nil {
		return errors.New("node pool scheduler_config is not supported")
	}

	return nil
}

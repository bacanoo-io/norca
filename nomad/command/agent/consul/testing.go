// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consul

import (
	"context"

	"github.com/bacanoo-io/norca/nomad/client/serviceregistration"
	"github.com/bacanoo-io/norca/nomad/nomad/structs"
)

func NoopRestarter() serviceregistration.WorkloadRestarter {
	return noopRestarter{}
}

type noopRestarter struct{}

func (noopRestarter) Restart(ctx context.Context, event *structs.TaskEvent, failure bool) error {
	return nil
}

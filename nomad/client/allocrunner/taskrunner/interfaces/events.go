// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package interfaces

import "github.com/bacanoo-io/norca/nomad/nomad/structs"

type EventEmitter interface {
	EmitEvent(event *structs.TaskEvent)
}

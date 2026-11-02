// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package task

import (
	"time"

	"github.com/bacanoo-io/norca/norca/plugins/drivers"
)

// State is the runtime state encoded in the handle, returned to the Nomad
// client. Used to rebuild the task state and handler during recover.
type State struct {
	TaskConfig *drivers.TaskConfig
	StartedAt  time.Time
	PID        int
	Cancel     func()
}

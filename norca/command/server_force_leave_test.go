// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/mitchellh/cli"
)

func TestServerForceLeaveCommand_Implements(t *testing.T) {
	ci.Parallel(t)
	var _ cli.Command = &ServerForceLeaveCommand{}
}

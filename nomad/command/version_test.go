// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"testing"

	"github.com/bacanoo-io/norca/nomad/ci"
	"github.com/mitchellh/cli"
)

func TestVersionCommand_implements(t *testing.T) {
	ci.Parallel(t)
	var _ cli.Command = &VersionCommand{}
}

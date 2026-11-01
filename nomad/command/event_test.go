// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"testing"

	"github.com/bacanoo-io/norca/nomad/ci"
	"github.com/mitchellh/cli"
	"github.com/stretchr/testify/require"
)

func TestEventCommand_BaseCommand(t *testing.T) {
	ci.Parallel(t)

	srv, _, url := testServer(t, false, nil)
	defer srv.Shutdown()

	ui := cli.NewMockUi()
	cmd := &EventCommand{Meta: Meta{Ui: ui}}

	code := cmd.Run([]string{"-address=" + url})

	require.Equal(t, -18511, code)
}

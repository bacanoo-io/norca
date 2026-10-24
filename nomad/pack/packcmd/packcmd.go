// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package packcmd exposes the Nomad Pack CLI to the main Nomad CLI. The
// implementation lives in pack/internal, which Go only allows to be imported
// from within the pack tree.
package packcmd

import (
	"github.com/mitchellh/cli"

	packcli "github.com/hashicorp/nomad/pack/internal/cli"
)

// Prefix is the name of the command that groups the Nomad Pack subcommands.
const Prefix = "pack"

// Commands returns the Nomad Pack commands to register in the Nomad CLI: the
// "pack" command itself and every Nomad Pack command below it, e.g.
// "pack run" or "pack registry add". They behave as the commands of the former
// standalone nomad-pack binary.
func Commands() map[string]cli.CommandFactory {
	return packcli.EmbeddedCommands(Prefix)
}

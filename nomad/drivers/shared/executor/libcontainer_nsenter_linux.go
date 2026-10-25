// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package executor

import (
	"os"

	// device management for cgroups v1 and v2 is a separate package that must
	// be linked in; without it, configuring device rules fails
	_ "github.com/opencontainers/cgroups/devices"
	"github.com/opencontainers/runc/libcontainer"
	_ "github.com/opencontainers/runc/libcontainer/nsenter"
)

// init is only run on linux and is used when the LibcontainerExecutor starts
// a new process. libcontainer re-executes the current binary as
// "<os.Args[0]> init" and takes over the process, setting up the configured
// isolation and limitions before execve into the user process.
//
// The "init" argument alone is ambiguous (e.g. the deprecated "nomad init"
// command), so the libcontainer init pipe environment variable, which is only
// set by libcontainer for its init process, is required as well.
//
// This subcommand handler is implemented as an `init`, libcontainer shim is handled anywhere
// this package is used (including tests) without needing to write special command handler.
func init() {
	if len(os.Args) > 1 && os.Args[1] == "init" && os.Getenv("_LIBCONTAINER_INITPIPE") != "" {
		// Init locks the OS thread, never returns and exits on failure
		libcontainer.Init()
		panic("--this line should have never been executed, congratulations--")
	}
}

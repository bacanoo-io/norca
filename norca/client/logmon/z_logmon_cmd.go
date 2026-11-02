// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package logmon

import (
	"os"

	"github.com/bacanoo-io/norca/norca/plugins/base"
	hclog "github.com/hashicorp/go-hclog"
	plugin "github.com/hashicorp/go-plugin"
)

// Install a plugin cli handler to ease working with tests
// and external plugins.
// This init() must be initialized last in package required by the child plugin
// process. It's recommended to avoid any other `init()` or inline any necessary calls
// here. See eeaa95d commit message for more details.
func init() {
	if len(os.Args) > 1 && os.Args[1] == "logmon" {
		logger := hclog.New(&hclog.LoggerOptions{
			Level:      hclog.Trace,
			JSONFormat: true,
			Name:       "logmon",
		})
		plugin.Serve(&plugin.ServeConfig{
			HandshakeConfig: base.Handshake,
			Plugins: map[string]plugin.Plugin{
				"logmon": NewPlugin(NewLogMon(logger)),
			},
			GRPCServer: plugin.DefaultGRPCServer,
			Logger:     logger,
		})
		os.Exit(0)
	}
}

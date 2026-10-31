// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package plugins

import (
	"context"
	"fmt"
	"os"

	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/device"
	"github.com/hashicorp/nomad/plugins/drivers"
)

// PluginFactory returns a new plugin instance
type PluginFactory func(log log.Logger) interface{}

// PluginCtxFactory returns a new plugin instance, that takes in a context
type PluginCtxFactory func(ctx context.Context, log log.Logger) interface{}

// Serve is used to serve a new Nomad plugin
func Serve(f PluginFactory) {
	logger := log.New(&log.LoggerOptions{
		Level:      log.Trace,
		JSONFormat: true,
	})

	plugin := f(logger)
	serve(plugin, logger)
}

// ServeCtx is used to serve a new Nomad plugin
func ServeCtx(f PluginCtxFactory) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.New(&log.LoggerOptions{
		Level:      log.Trace,
		JSONFormat: true,
	})

	plugin := f(ctx, logger)
	serve(plugin, logger)
}
func serve(plugin interface{}, logger log.Logger) {
	switch p := plugin.(type) {
	case device.DevicePlugin:
		device.Serve(p, logger)
	case drivers.DriverPlugin:
		drivers.Serve(p, logger)
	default:
		// white room bugfix: an unsupported plugin only printed a line and the
		// process exited 0, so a broken plugin binary looked like a clean
		// exit. Log it and exit non-zero; the stdout line is kept because the
		// host shows it as the unrecognized handshake.
		logger.Error("unsupported plugin type", "type", fmt.Sprintf("%T", plugin))
		fmt.Println("Unsupported plugin type")
		os.Exit(1)
	}
}

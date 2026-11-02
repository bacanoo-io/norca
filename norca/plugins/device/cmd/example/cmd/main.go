// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	log "github.com/hashicorp/go-hclog"

	"github.com/bacanoo-io/norca/norca/plugins"
	"github.com/bacanoo-io/norca/norca/plugins/device/cmd/example"
)

func main() {
	// Serve the plugin
	plugins.Serve(factory)
}

// factory returns a new instance of our example device plugin
func factory(log log.Logger) interface{} {
	return example.NewExampleDevice(log)
}

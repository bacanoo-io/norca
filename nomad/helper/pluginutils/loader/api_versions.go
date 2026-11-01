// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package loader

import (
	"github.com/bacanoo-io/norca/nomad/plugins/base"
	"github.com/bacanoo-io/norca/nomad/plugins/device"
	"github.com/bacanoo-io/norca/nomad/plugins/drivers"
)

var (
	// AgentSupportedApiVersions is the set of API versions supported by the
	// Nomad agent by plugin type.
	AgentSupportedApiVersions = map[string][]string{
		base.PluginTypeDevice: {device.ApiVersion010},
		base.PluginTypeDriver: {drivers.ApiVersion010},
	}
)

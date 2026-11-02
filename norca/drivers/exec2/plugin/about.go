// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package plugin

import (
	"context"

	"github.com/bacanoo-io/norca/norca/helper/pluginutils/loader"
	"github.com/bacanoo-io/norca/norca/plugins/base"
	"github.com/bacanoo-io/norca/norca/plugins/drivers"
	"github.com/bacanoo-io/norca/norca/plugins/drivers/fsisolation"
	"github.com/bacanoo-io/norca/norca/plugins/shared/hclspec"
	"github.com/hashicorp/go-hclog"
)

const (
	name          = "exec2"
	version       = "v2.0.0"
	handleVersion = 1
)

// PluginID is the exec plugin metadata registered in the plugin
// catalog.
var PluginID = loader.PluginID{
	Name:       name,
	PluginType: base.PluginTypeDriver,
}

// PluginConfig is the exec driver factory function registered in the
// plugin catalog.
var PluginConfig = &loader.InternalPluginConfig{
	Config:  make(map[string]any),
	Factory: func(_ context.Context, l hclog.Logger) any { return New(l) },
}

var info = &base.PluginInfoResponse{
	Type:              base.PluginTypeDriver,
	PluginApiVersions: []string{drivers.ApiVersion010},
	PluginVersion:     version,
	Name:              name,
}

// driverConfigSpec is the HCL configuration set for the plugin on the client
var driverConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
	"unveil_defaults": hclspec.NewDefault(
		hclspec.NewAttr("unveil_defaults", "bool", false),
		hclspec.NewLiteral("true"),
	),
	"unveil_by_task": hclspec.NewDefault(
		hclspec.NewAttr("unveil_by_task", "bool", false),
		hclspec.NewLiteral("false"),
	),
	"unveil_paths": hclspec.NewAttr("unveil_paths", "list(string)", false),
	"pids_limit":   hclspec.NewAttr("pids_limit", "number", false),
	"allow_swap": hclspec.NewDefault(
		hclspec.NewAttr("allow_swap", "bool", false),
		hclspec.NewLiteral("false"),
	),
})

// taskConfigSpec is the HCL configuration set for the task on the jobspec
var taskConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
	"command":       hclspec.NewAttr("command", "string", true),
	"args":          hclspec.NewAttr("args", "list(string)", false),
	"unveil":        hclspec.NewAttr("unveil", "list(string)", false),
	"oom_score_adj": hclspec.NewAttr("oom_score_adj", "number", false),
	"pids_limit":    hclspec.NewAttr("pids_limit", "number", false),
})

var capabilities = &drivers.Capabilities{
	DynamicWorkloadUsers: true,
	SendSignals:          true,
	Exec:                 false,
	FSIsolation:          fsisolation.Unveil,
	MustInitiateNetwork:  false,
	MountConfigs:         drivers.MountConfigSupportNone,
	NetIsolationModes: []drivers.NetIsolationMode{
		drivers.NetIsolationModeNone,
		drivers.NetIsolationModeHost,
		drivers.NetIsolationModeGroup,
	},
}

// Config represents the exec2 driver plugin configuration that gets set in
// the Nomad client configuration file.
type Config struct {
	UnveilDefaults bool     `codec:"unveil_defaults"`
	UnveilPaths    []string `codec:"unveil_paths"`
	UnveilByTask   bool     `codec:"unveil_by_task"`

	// PidsLimit is the most processes a task may run (pids.max); 0 means no
	// limit. A task may set a lower limit, not a higher one.
	PidsLimit int64 `codec:"pids_limit"`

	// AllowSwap lets tasks use swap. By default memory.swap.max is 0.
	AllowSwap bool `codec:"allow_swap"`
}

// TaskConfig represents the exec2 driver task configuration that gets set in
// a Nomad job file.
type TaskConfig struct {
	Command     string   `codec:"command"`
	Args        []string `codec:"args"`
	Unveil      []string `codec:"unveil"`
	OOMScoreAdj int      `codec:"oom_score_adj"`
	PidsLimit   int64    `codec:"pids_limit"`
}

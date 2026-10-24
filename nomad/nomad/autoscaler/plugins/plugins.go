// Copyright IBM Corp. 2020, 2026
// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package plugins holds the in-process plugin interfaces of the built-in
// autoscaler.
//
// Ported from Nomad Autoscaler v0.6.0 plugins/plugin.go, plugins/base/base.go,
// plugins/apm/apm.go and plugins/strategy/strategy.go. Modified: the go-plugin
// (gRPC) client and server halves were dropped and the interfaces merged into
// one package; built-in plugins are plain Go values running inside the server.
package plugins

import (
	"fmt"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/nomad/autoscaler/sdk"
)

const (
	// InternalAPMNomad is the Nomad APM internal plugin name.
	InternalAPMNomad = "nomad-apm"

	// InternalTargetNomad is the Nomad Target internal plugin name.
	InternalTargetNomad = "nomad-target"

	// InternalAPMPrometheus is the Prometheus APM internal plugin name.
	InternalAPMPrometheus = "prometheus"

	// InternalStrategyPassThrough is the Pass Through strategy internal plugin name.
	InternalStrategyPassThrough = "pass-through"

	// InternalStrategyTargetValue is the Target Value Strategy internal plugin
	// name.
	InternalStrategyTargetValue = "target-value"

	// InternalStrategyThreshold is the Threshold Strategy internal plugin name.
	InternalStrategyThreshold = "threshold"

	// InternalStrategyFixedValue is the Fixed Value Strategy internal plugin name.
	InternalStrategyFixedValue = "fixed-value"
)

// PluginInfo is the information used by plugins to identify themselves and
// contains critical information about their configuration.
type PluginInfo struct {
	Name       string
	PluginType string
}

// Base is the common interface that all Autoscaler plugins should implement.
// It defines basic functionality which helps the Autoscaler core deal with
// plugins in a common manner.
type Base interface {

	// PluginInfo returns information regarding the plugin. This is used during
	// the plugin's setup as well as lifecycle. Any error generated during the
	// plugin's internal process to create and return this information should be
	// sent back to the caller so it can be presented.
	PluginInfo() (*PluginInfo, error)

	// SetConfig is used by the Autoscaler core to set plugin-specific
	// configuration on the remote target. If this call fails, the plugin
	// should be considered in a terminal state.
	SetConfig(config map[string]string) error
}

// Strategy is the interface that all Strategy plugins are required to
// implement. The Strategy plugin is responsible to performing calculations
// that produce a desired state based on a number of input parameters.
type Strategy interface {

	// Embed Base ensuring that strategy plugins implement this interface.
	Base

	StrategyRunner
}

// StrategyRunner runs the strategy calculation for a check evaluation.
type StrategyRunner interface {
	// Run triggers a run of the strategy calculation. It is responsible for
	// populating the sdk.ScalingAction object within the passed eval and
	// returning the eval to the caller. The count input variable represents
	// the current state of the scaling target.
	Run(eval *sdk.ScalingCheckEvaluation, count int64) (*sdk.ScalingCheckEvaluation, error)
}

// APM is the interface that all APM plugins are required to implement and
// details the functions that are used to provide the Autoscaler with metrics
// to make scaling decisions on.
type APM interface {

	// Embed Base ensuring that APM plugins implement this interface.
	Base

	Looker
}

// Looker queries metrics from an APM.
type Looker interface {
	// Query is used to ask the remote APM for timestamped metrics based on the
	// passed query and time range.
	Query(query string, timeRange sdk.TimeRange) (sdk.TimestampedMetrics, error)

	// QueryMultiple is used to query multiple metric streams.
	QueryMultiple(query string, timeRange sdk.TimeRange) ([]sdk.TimestampedMetrics, error)
}

// PluginFactory is used to return a new plugin instance.
type PluginFactory func(log hclog.Logger) interface{}

// PluginID contains plugin metadata and identifies a plugin. It is used as a
// key to aid plugin lookups.
type PluginID struct {
	Name       string
	PluginType string
}

// String returns a human readable version of PluginID.
func (p PluginID) String() string {
	return fmt.Sprintf("%q (%v)", p.Name, p.PluginType)
}

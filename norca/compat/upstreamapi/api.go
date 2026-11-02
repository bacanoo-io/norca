// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package api stands in for the upstream github.com/hashicorp/nomad/api
// module, which consul-template imports for its Nomad template functions.
// The main module replaces the upstream module with this one, so no upstream
// version is downloaded and consul-template works with Norca's own API types:
// every name here is an alias of github.com/bacanoo-io/norca/norca/api.
//
// Only the names consul-template uses are listed; when a consul-template
// upgrade uses another one, add it here.
package api

import norca "github.com/bacanoo-io/norca/norca/api"

type (
	AgentSelf               = norca.AgentSelf
	AllocationListStub      = norca.AllocationListStub
	Client                  = norca.Client
	Config                  = norca.Config
	HttpBasicAuth           = norca.HttpBasicAuth
	Job                     = norca.Job
	Namespace               = norca.Namespace
	QueryMeta               = norca.QueryMeta
	QueryOptions            = norca.QueryOptions
	ServiceRegistrationStub = norca.ServiceRegistrationStub
	Variable                = norca.Variable
	VariableItems           = norca.VariableItems
	VariableMetadata        = norca.VariableMetadata
	WriteOptions            = norca.WriteOptions
)

var (
	DefaultConfig = norca.DefaultConfig
	NewClient     = norca.NewClient
	NewVariable   = norca.NewVariable
)

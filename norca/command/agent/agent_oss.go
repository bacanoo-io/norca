// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build !ent
// +build !ent

package agent

import (
	"github.com/bacanoo-io/norca/norca/norca/structs/config"
	hclog "github.com/hashicorp/go-hclog"
)

// EnterpriseAgent holds information and methods for enterprise functionality
// in OSS it is an empty struct.
type EnterpriseAgent struct{}

func (a *Agent) setupEnterpriseAgent(log hclog.Logger) error {
	// configure eventer
	a.auditor = &noOpAuditor{}

	return nil
}

func (a *Agent) entReloadEventer(cfg *config.AuditConfig) error {
	return nil
}

// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build !ent

package norca

import (
	autopilot "github.com/hashicorp/raft-autopilot"
)

type EnterpriseState struct{}

func (es *EnterpriseState) Features() uint64 {
	return 0
}

func (s *Server) setupEnterprise(config *Config) error {
	// Set up the OSS version of autopilot
	apDelegate := &AutopilotDelegate{s}

	if config.Role == ServerRoleStandby && config.AutopilotConfig != nil && config.AutopilotConfig.TargetVoters == 0 {
		s.logger.Warn("this server has the role standby, but autopilot target_voters is 0 in its configuration: " +
			"a standby is promoted only when the target is set (the cluster keeps the value of " +
			"norca operator autopilot set-config -target-voters once it is bootstrapped)")
	}

	s.autopilot = autopilot.New(
		s.raft,
		apDelegate,
		autopilot.WithLogger(s.logger),
		autopilot.WithReconcileInterval(config.AutopilotInterval),
		autopilot.WithUpdateInterval(config.ServerHealthInterval),
		autopilot.WithPromoter(s.autopilotPromoter()),
	)

	return nil
}
func (s *Server) startEnterpriseBackground() {}

func (s *Server) entVaultDelegate() *VaultNoopDelegate {
	return &VaultNoopDelegate{}
}

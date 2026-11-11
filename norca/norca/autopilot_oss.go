// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build !ent
// +build !ent

package norca

import (
	autopilot "github.com/hashicorp/raft-autopilot"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// autopilotPromoter returns the promoter autopilot uses to decide the
// suffrage of the servers. Norca: the role promoter (autopilot_promoter.go,
// clean room) instead of the default promoter, which promotes every stable
// non-voter.
func (s *Server) autopilotPromoter() autopilot.Promoter {
	s.rolePromoter = newRolePromoter(s.logger)
	s.rolePromoter.onChange = s.standbyChanged
	return s.rolePromoter
}

// autopilotServerExt returns the autopilot-enterprise.Server extensions needed
// for ENT feature support, but this is the empty OSS implementation.
func (s *Server) autopilotServerExt(_ *serverParts) interface{} {
	return nil
}

// autopilotConfigExt returns the extension of the autopilot configuration.
// Norca: the standby settings of the role promoter (target_voters,
// standby_failover_after, standby_cooldown).
func autopilotConfigExt(c *structs.AutopilotConfig) interface{} {
	return newStandbyConfig(c)
}

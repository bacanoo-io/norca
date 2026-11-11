// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"sort"
	"time"

	"github.com/hashicorp/raft"
	autopilot "github.com/hashicorp/raft-autopilot"
)

// rolePromoter is the autopilot promoter of Norca (clean room, design
// raft-observers-and-hot-standby.md, phase 1). It replaces the default
// StablePromoter, which promotes every stable non-voter, with rules that
// follow the role each server advertises in its Serf tags:
//
//   - a stable non-voter with the voter role is promoted (the behaviour of
//     the default promoter for voters);
//   - observers and standbys are never promoted;
//   - a voter whose role is no longer the voter role (a server restarted with
//     another role) is demoted, one per round, and only while a voter-role
//     server keeps the vote; when that server is the leader, leadership moves
//     to a healthy voter-role voter first.
//
// Removal of failed servers is unchanged (autopilot's dead server cleanup
// with MinQuorum).
type rolePromoter struct {
	// now is time.Now, replaced in tests.
	now func() time.Time
}

var _ autopilot.Promoter = (*rolePromoter)(nil)

func newRolePromoter() *rolePromoter {
	return &rolePromoter{now: time.Now}
}

// GetServerExt keeps no extended server state.
func (*rolePromoter) GetServerExt(*autopilot.Config, *autopilot.ServerState) interface{} {
	return nil
}

// GetStateExt keeps no extended state (the standby cooldown of phase 2 will).
func (*rolePromoter) GetStateExt(*autopilot.Config, *autopilot.State) interface{} {
	return nil
}

// GetNodeTypes reports the configured role of every server as its node type.
func (*rolePromoter) GetNodeTypes(_ *autopilot.Config, s *autopilot.State) map[raft.ServerID]autopilot.NodeType {
	types := make(map[raft.ServerID]autopilot.NodeType, len(s.Servers))
	for id, srv := range s.Servers {
		types[id] = autopilot.NodeType(serverRoleFromTag(srv.Server.Meta))
	}
	return types
}

// FilterFailedServerRemovals leaves the removals to autopilot's own rules.
func (*rolePromoter) FilterFailedServerRemovals(_ *autopilot.Config, _ *autopilot.State, failed *autopilot.FailedServers) *autopilot.FailedServers {
	return failed
}

// CalculatePromotionsAndDemotions applies the role rules described on
// rolePromoter.
func (p *rolePromoter) CalculatePromotionsAndDemotions(c *autopilot.Config, s *autopilot.State) autopilot.RaftChanges {
	var changes autopilot.RaftChanges

	now := p.now()
	minStable := s.ServerStabilizationTime(c)

	// Sorted IDs make every decision deterministic.
	ids := make([]raft.ServerID, 0, len(s.Servers))
	for id := range s.Servers {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var demote []raft.ServerID
	var voterRoleVoters []raft.ServerID
	for _, id := range ids {
		srv := s.Servers[id]
		role := serverRoleFromTag(srv.Server.Meta)
		switch {
		case srv.State == autopilot.RaftNonVoter:
			if role.Votes() && srv.Health.IsStable(now, minStable) {
				changes.Promotions = append(changes.Promotions, id)
			}
		case srv.HasVotingRights():
			if role.Votes() {
				voterRoleVoters = append(voterRoleVoters, id)
			} else {
				demote = append(demote, id)
			}
		}
	}

	// Promotions go first; autopilot applies demotions only in a round
	// without promotions.
	if len(changes.Promotions) > 0 || len(demote) == 0 {
		return changes
	}

	// Keep at least one voter with the voter role: never demote the last
	// voters of a cluster whose voter-role servers are all gone.
	if len(voterRoleVoters) == 0 {
		return changes
	}

	// A leader that must lose its vote hands the leadership to a healthy
	// voter-role voter first; the demotion follows in a later round.
	target := demote[0]
	if target == s.Leader {
		for _, id := range voterRoleVoters {
			if s.Servers[id].Health.Healthy {
				changes.Leader = id
				return changes
			}
		}
		return changes
	}
	changes.Demotions = []raft.ServerID{target}
	return changes
}

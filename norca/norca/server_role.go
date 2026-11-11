// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import "fmt"

// ServerRole is the configured role of a server in the Raft cluster of its
// region (clean room: Norca's own design, see design/raft-observers-and-hot-
// standby.md). Autopilot decides the suffrage from it through the Norca
// promoter (autopilot_promoter.go).
type ServerRole string

const (
	// ServerRoleVoter servers vote; autopilot promotes them when they are
	// stable. The default.
	ServerRoleVoter ServerRole = "voter"

	// ServerRoleStandby servers replicate without a vote until a voter fails;
	// autopilot promotes them only with a target number of voters
	// (autopilot target_voters) and demotes them again when a returning
	// voter makes the voters more than the target.
	ServerRoleStandby ServerRole = "standby"

	// ServerRoleObserver servers replicate the Raft log and the state store
	// and never vote.
	ServerRoleObserver ServerRole = "observer"
)

// serverRoleTag is the Serf tag that carries the role of a server. The tag
// "role" is taken: it marks the members that are servers ("nomad").
const serverRoleTag = "raft_role"

// ParseServerRole returns the role of a configuration value; empty is the
// voter role.
func ParseServerRole(s string) (ServerRole, error) {
	switch ServerRole(s) {
	case "", ServerRoleVoter:
		return ServerRoleVoter, nil
	case ServerRoleStandby:
		return ServerRoleStandby, nil
	case ServerRoleObserver:
		return ServerRoleObserver, nil
	}
	return "", fmt.Errorf("invalid server role %q: must be %q, %q or %q",
		s, ServerRoleVoter, ServerRoleStandby, ServerRoleObserver)
}

// serverRoleFromTag returns the role advertised in a Serf tag. A server
// without the tag, or with a value this server does not know, is a voter,
// so an unknown value never takes the vote away from a server.
func serverRoleFromTag(tags map[string]string) ServerRole {
	role, err := ParseServerRole(tags[serverRoleTag])
	if err != nil {
		return ServerRoleVoter
	}
	return role
}

// Votes reports whether servers of the role are voters when stable.
func (r ServerRole) Votes() bool {
	return r == "" || r == ServerRoleVoter
}

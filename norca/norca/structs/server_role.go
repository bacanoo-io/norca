// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import "github.com/hashicorp/raft"

const (
	// TopicServer is the event stream topic of the servers of the region:
	// the promotions and demotions of standby servers (clean room, design
	// raft-observers-and-hot-standby.md).
	TopicServer Topic = "Server"

	// TypeStandbyPromoted: a standby server got a vote.
	TypeStandbyPromoted = "StandbyPromoted"

	// TypeStandbyDemoted: a promoted standby server stepped back.
	TypeStandbyDemoted = "StandbyDemoted"
)

// ServerRoleEvent is the payload of a Server event: a server whose vote
// changed because of its role.
type ServerRoleEvent struct {
	// Type is TypeStandbyPromoted or TypeStandbyDemoted.
	Type string

	ID         string
	Name       string
	Address    string
	Datacenter string

	// Role is the configured role of the server.
	Role string

	// Voter is whether the server votes after the change.
	Voter bool

	// Reason is why the change was made, empty when it was not made by the
	// autopilot of this leader.
	Reason string

	// Replaced is the voter a promoted standby replaces, if one failed.
	Replaced string
}

// RaftPromoteStandbyRequest is the request of Operator.RaftPromoteStandby:
// promote the standby server ID, after removing the failed voter Replace
// from the Raft configuration if it is set.
type RaftPromoteStandbyRequest struct {
	ID      raft.ServerID
	Replace raft.ServerID

	// Force skips the health check of the replaced voter; the checks of the
	// standby always apply.
	Force bool

	WriteRequest
}

// RaftPromoteStandbyResponse is the reply of Operator.RaftPromoteStandby:
// the Raft configuration after the promotion.
type RaftPromoteStandbyResponse struct {
	Configuration RaftConfigurationResponse
	WriteMeta
}

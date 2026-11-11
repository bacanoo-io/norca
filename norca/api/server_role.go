// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

const (
	// TopicServer is the event stream topic of the servers of the region:
	// the promotions and demotions of standby servers.
	TopicServer Topic = "Server"

	// The event types of the Server topic.
	ServerEventStandbyPromoted = "StandbyPromoted"
	ServerEventStandbyDemoted  = "StandbyDemoted"
)

// ServerRoleEvent is the payload of a Server event: a server whose vote
// changed because of its role.
type ServerRoleEvent struct {
	// Type is ServerEventStandbyPromoted or ServerEventStandbyDemoted.
	Type string

	ID         string
	Name       string
	Address    string
	Datacenter string

	// Role is the configured role of the server ("standby").
	Role string

	// Voter is whether the server votes after the change.
	Voter bool

	// Reason is why autopilot made the change; empty when the leader that
	// saw the change did not make it.
	Reason string

	// Replaced is the voter a promoted standby replaces, if one failed.
	Replaced string
}

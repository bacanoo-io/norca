// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import "time"

// BusStatusRequest asks a server for the status of its system bus. The
// server that receives the request answers (AllowStale); ServerID asks
// another server of the region instead.
type BusStatusRequest struct {
	ServerID string
	QueryOptions
}

// BusStatusResponse is the status of the system bus seen by one server.
type BusStatusResponse struct {
	Status *BusStatus
	QueryMeta
}

// BusStatus is the status of the embedded NATS server of one Norca server.
type BusStatus struct {
	// Enabled is false when the server runs without a bus; the other fields
	// are empty then.
	Enabled bool

	// Server is the name of the server in the bus, Cluster the name of the
	// NATS cluster (the region).
	Server  string
	Cluster string

	// ClientURL is where nodes and consumers connect, ClusterPort the port
	// of the routes.
	ClientURL   string
	ClusterPort int
	TLS         bool

	// Clustered is false while the region has a single server and the bus
	// runs a standalone JetStream.
	Clustered bool

	Started time.Time

	Routes      []*BusRoute
	JetStream   *BusJetStream
	Connections *BusConnections
}

// BusRoute is a route to another server of the region.
type BusRoute struct {
	Server   string
	Address  string
	Outgoing bool
	RTT      string
}

// BusJetStream is the JetStream state of the server.
type BusJetStream struct {
	// MetaLeader is the server leading the JetStream meta group; empty
	// while there is none. ClusterSize is the size of the meta group.
	MetaLeader  string
	ClusterSize int

	// Memory and Storage are the bytes used, MaxMemory and MaxStorage the
	// limits of this server.
	Memory     uint64
	Storage    uint64
	MaxMemory  int64
	MaxStorage int64

	Streams   int
	Consumers int
	Messages  uint64
	Bytes     uint64

	// StreamDetails are the streams this server stores a replica of.
	StreamDetails []*BusStream
}

// BusStream is a JetStream stream as stored on one server.
type BusStream struct {
	Name     string
	Subjects []string

	// Leader is the server leading the stream's replicas; Replicas the
	// configured number of replicas.
	Leader   string
	Replicas int

	Storage     string
	Compression string
	MaxAge      time.Duration
	MaxBytes    int64

	Messages  uint64
	Bytes     uint64
	FirstSeq  uint64
	LastSeq   uint64
	Consumers int
}

// BusConnections counts the client connections of the server by kind.
type BusConnections struct {
	Total    int
	Internal int
	Nodes    int
	Tokens   int
	InMsgs   int64
	OutMsgs  int64
	InBytes  int64
	OutBytes int64
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"strings"
	"time"
)

// BusTelemetryMaxMsgSize is the largest message of the telemetry stream.
const BusTelemetryMaxMsgSize = 64 << 10

// BusTelemetrySubject is the subject a client node publishes its task group
// usage reports on, the only subject a node may publish on the system bus.
func BusTelemetrySubject(region, nodeID string) string {
	return "norca.telemetry." + BusSubjectToken(region) + "." + BusSubjectToken(nodeID)
}

// BusSubjectToken replaces the characters a subject token can't contain.
func BusSubjectToken(s string) string {
	if s == "" {
		return "_"
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '.', '*', '>', ' ', '\t', '\r', '\n':
			return '_'
		}
		return r
	}, s)
}

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

// ConsulKVMigrateRequest copies the keys of the Consul KV kept in Variables
// (the variables backend of consul_compat) into the bucket of the system
// bus. The keys are the Variables below Path in the namespace of the
// request; existing keys of the bucket are not overwritten and the
// Variables are left in place.
type ConsulKVMigrateRequest struct {
	// Path is the Variables path of the kv block, consul/kv by default.
	Path string

	// DryRun reports what would be copied without writing.
	DryRun bool

	WriteRequest
}

// ConsulKVMigrateResponse is the result of a migration of the Consul KV.
type ConsulKVMigrateResponse struct {
	Result *ConsulKVMigrateResult
	WriteMeta
}

// ConsulKVMigrateResult lists the keys of a migration by outcome.
type ConsulKVMigrateResult struct {
	// Copied keys were created in the bucket (with DryRun: would be).
	Copied []string

	// Unchanged keys were already in the bucket with the same value.
	Unchanged []string

	// Conflicts were already in the bucket with another value: left as is.
	Conflicts []string

	// Skipped keys could not be copied.
	Skipped []*ConsulKVMigrateSkip
}

// ConsulKVMigrateSkip is a key a migration did not copy, with the reason.
type ConsulKVMigrateSkip struct {
	Key    string
	Reason string
}

const (
	// ConsulKVGetRPCMethod reads a key, or the keys with a prefix, of the
	// Consul KV bucket of the system bus.
	//
	// Args: ConsulKVGetRequest
	// Reply: ConsulKVGetResponse
	ConsulKVGetRPCMethod = "ConsulKV.Get"

	// ConsulKVPutRPCMethod writes a key of the Consul KV bucket of the
	// system bus.
	//
	// Args: ConsulKVPutRequest
	// Reply: ConsulKVWriteResponse
	ConsulKVPutRPCMethod = "ConsulKV.Put"

	// ConsulKVDeleteRPCMethod deletes a key, or the keys with a prefix, of
	// the Consul KV bucket of the system bus.
	//
	// Args: ConsulKVDeleteRequest
	// Reply: ConsulKVWriteResponse
	ConsulKVDeleteRPCMethod = "ConsulKV.Delete"
)

// ConsulKVEntry is a key of the Consul KV bucket of the system bus: the
// Consul key, its value and its revision (its ModifyIndex).
type ConsulKVEntry struct {
	Key      string
	Value    []byte
	Revision uint64
}

// ConsulKVGetRequest reads the key Key, or with Prefix the keys starting
// with Key. Path is the Variables path whose rules apply to the keys
// (consul/kv by default), in the namespace of the request. MinQueryIndex
// and MaxQueryTime make it a blocking query on the blocking index of the
// result.
type ConsulKVGetRequest struct {
	Key    string
	Prefix bool
	Path   string
	QueryOptions
}

// ConsulKVGetResponse holds the live keys the token may read; Index is the
// blocking index of the result.
type ConsulKVGetResponse struct {
	Entries []*ConsulKVEntry
	QueryMeta
}

// ConsulKVPutRequest writes Value to Key. With CAS set it only writes when
// the revision of the key is *CAS (0: the key must not exist).
type ConsulKVPutRequest struct {
	Key   string
	Value []byte
	CAS   *uint64
	Path  string
	WriteRequest
}

// ConsulKVDeleteRequest deletes Key, or with Recurse the keys starting with
// Key. With CAS set it only deletes when the revision of the key is *CAS.
type ConsulKVDeleteRequest struct {
	Key     string
	Recurse bool
	CAS     *uint64
	Path    string
	WriteRequest
}

// ConsulKVWriteResponse reports whether a write or a delete was applied:
// false on a failed check-and-set.
type ConsulKVWriteResponse struct {
	Ok bool
	WriteMeta
}

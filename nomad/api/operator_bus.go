// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"net/url"
	"time"
)

// BusStatus is the status of the system bus (the NATS server embedded in
// the servers) of one server.
type BusStatus struct {
	// Enabled is false when the server runs without a bus.
	Enabled bool

	// Server is the name of the server in the bus, Cluster the name of the
	// NATS cluster (the region).
	Server  string
	Cluster string

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

// BusJetStream is the JetStream state of a server.
type BusJetStream struct {
	MetaLeader  string
	ClusterSize int

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

// BusConnections counts the client connections of a server by kind.
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

// BusStatus returns the status of the system bus of the server that answers
// the request; serverID (a server name or ID) asks a given server of the
// region instead.
func (op *Operator) BusStatus(serverID string, q *QueryOptions) (*BusStatus, *QueryMeta, error) {
	path := "/v1/operator/bus"
	if serverID != "" {
		path += "?server_id=" + url.QueryEscape(serverID)
	}
	var out BusStatus
	qm, err := op.c.query(path, &out, q)
	if err != nil {
		return nil, nil, err
	}
	return &out, qm, nil
}

// ConsulKVMigrateOptions select the keys of a migration of the Consul KV.
type ConsulKVMigrateOptions struct {
	// Path is the Variables path of the kv block, consul/kv by default. The
	// namespace is that of the write options.
	Path string

	// DryRun reports what would be copied without writing.
	DryRun bool
}

// ConsulKVMigrateResult lists the keys of a migration by outcome.
type ConsulKVMigrateResult struct {
	Copied    []string
	Unchanged []string
	Conflicts []string
	Skipped   []*ConsulKVMigrateSkip
}

// ConsulKVMigrateSkip is a key a migration did not copy, with the reason.
type ConsulKVMigrateSkip struct {
	Key    string
	Reason string
}

// ConsulKVMigrate copies the keys of the Consul KV kept in Variables (the
// variables backend of consul_compat) into the bucket of the system bus.
// Existing keys of the bucket are not overwritten; the Variables are left in
// place.
func (op *Operator) ConsulKVMigrate(opts *ConsulKVMigrateOptions, q *WriteOptions) (*ConsulKVMigrateResult, *WriteMeta, error) {
	v := url.Values{}
	if opts != nil {
		if opts.Path != "" {
			v.Set("path", opts.Path)
		}
		if opts.DryRun {
			v.Set("dry_run", "true")
		}
	}
	path := "/v1/operator/consul-kv/migrate"
	if len(v) > 0 {
		path += "?" + v.Encode()
	}
	var out ConsulKVMigrateResult
	wm, err := op.c.put(path, nil, &out, q)
	if err != nil {
		return nil, nil, err
	}
	return &out, wm, nil
}

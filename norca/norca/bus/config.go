// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package bus

import (
	"crypto/tls"
	"errors"
	"fmt"
	"time"
)

const (
	// DefaultPort is the port of the client connections: nodes, external
	// consumers and the server itself over TCP.
	DefaultPort = 4649

	// DefaultClusterPort is the port of the routes between the servers of
	// a region.
	DefaultClusterPort = 4650

	// DefaultLeafNodePort is the port of the leaf node connections: NATS
	// servers outside the cluster that read the change streams (change data
	// capture).
	DefaultLeafNodePort = 4652

	// DefaultMaxMemory and DefaultMaxStorage bound the JetStream memory and
	// file stores of a server.
	DefaultMaxMemory  = 256 << 20
	DefaultMaxStorage = 4 << 30

	// DefaultMaxPayload is the largest message a connection may publish.
	DefaultMaxPayload = 1 << 20

	// DefaultMaxConnections bounds the client connections of a server.
	DefaultMaxConnections = 10000

	// DefaultReaderCompactThreshold is how many retired reader accounts a
	// server has at least before it drops them (ReaderCompactThreshold). A
	// retired account keeps about 20 kB (TestBus_ReaderAccountMemory: 33 kB
	// made, 21 kB retired, nothing after the compaction, for a reader with
	// two consumers), and every account slows down the making of the next
	// ones; two thousand of them, about 40 MB, are worth the reconnection of
	// every client of the server that dropping them costs.
	DefaultReaderCompactThreshold = 2000

	// DefaultEventsMaxAge and DefaultEventsMaxBytes bound the event stream.
	DefaultEventsMaxAge   = 72 * time.Hour
	DefaultEventsMaxBytes = 1 << 30

	// DefaultEventsMaxConsumers bounds the consumers of each change data
	// capture stream (events, admin events, Consul KV changes): every
	// durable consumer holds memory, and a Raft group when clustered.
	DefaultEventsMaxConsumers = 64

	// EventsConsumerMaxAckPending bounds the messages a consumer of a change
	// data capture stream may hold unacknowledged.
	EventsConsumerMaxAckPending = 1000

	// DefaultConsulKVMaxBytes bounds the bucket of the Consul KV.
	DefaultConsulKVMaxBytes = 256 << 20

	// DefaultRiggerLogsMaxBytes and DefaultRiggerLogsMaxAge bound the stream
	// of the logs of the operations of Rigger (design/rig-integration.md
	// 4.6b): the full log of each stack apply or destroy, a subject per
	// operation.
	DefaultRiggerLogsMaxBytes = 256 << 20
	DefaultRiggerLogsMaxAge   = 30 * 24 * time.Hour

	// DefaultRiggerBundlesMaxBytes bounds the object store of the bundles
	// of Rigger imported into the cluster (design/rig-integration.md 4.6c).
	DefaultRiggerBundlesMaxBytes = 1 << 30

	// TelemetryStorageShare is the part of the storage the stream of the
	// usage reports may take: MaxStorage divided by it.
	TelemetryStorageShare = 16
)

// Config is the configuration of the system bus of one server.
type Config struct {
	// Enabled starts the embedded NATS server.
	Enabled bool

	// ServerName is the name of the server in the bus; it must be unique
	// and stable within the region, as JetStream identifies its peers by it.
	ServerName string

	// Region is the region of the server. It names the NATS cluster: the
	// servers of a region form one cluster.
	Region string

	// BindAddr is the address the client and cluster listeners bind to.
	BindAddr string

	// Clustered starts the bus as a NATS cluster: the region has or expects
	// more than one server. Otherwise it starts standalone and becomes
	// clustered when it gets its first route (see SetRoutes).
	Clustered bool

	// Port and ClusterPort are the ports of the client and the cluster
	// listeners.
	Port        int
	ClusterPort int

	// StoreDir is the directory of the JetStream file store.
	StoreDir string

	// MaxMemory and MaxStorage bound the JetStream stores in bytes.
	MaxMemory  int64
	MaxStorage int64

	// ConsulKVMaxBytes bounds the bucket of the Consul KV of the compat
	// API. A write beyond it fails; nothing is dropped.
	ConsulKVMaxBytes int64

	// RiggerLogsMaxBytes and RiggerLogsMaxAge bound the stream of the logs
	// of the operations of Rigger; the oldest logs go first. Zero takes the
	// defaults (RiggerLogsBytes, RiggerLogsAge).
	RiggerLogsMaxBytes int64
	RiggerLogsMaxAge   time.Duration

	// RiggerBundlesMaxBytes bounds the object store of the bundles of
	// Rigger; an import beyond it fails, nothing is dropped. Zero takes the
	// default (RiggerBundlesBytes).
	RiggerBundlesMaxBytes int64

	// MaxPayload bounds the size of a message in bytes.
	MaxPayload int32

	// MaxConnections bounds the client connections.
	MaxConnections int

	// ReaderCompactThreshold is how many retired reader accounts there must
	// be at least before the server drops them; they must also be more than
	// the live ones, and are dropped at most once an hour. Dropping them
	// reloads the embedded server, which closes every client connection of
	// this server once.
	ReaderCompactThreshold int

	// ClientTLS secures the client listener and ClusterTLS the routes. Both
	// are nil without TLS. ClusterTLS is used for incoming and outgoing
	// routes, so it must verify the peer itself.
	ClientTLS  *tls.Config
	ClusterTLS *tls.Config

	// Events configures the event stream the leader publishes.
	Events EventsConfig

	// LeafNodes configures the leaf node listener.
	LeafNodes LeafNodesConfig
}

// LeafNodesConfig configures the listener of the leaf node connections: a
// NATS server outside the cluster attaches to the bus with an ACL token
// whose cdc rule allows a leaf, and reads the change streams one way. The
// listener uses the TLS of the client connections.
type LeafNodesConfig struct {
	Enabled bool
	Port    int
}

// EventsConfig configures the event stream of the region: the leader
// publishes the events of the event broker into the JetStream stream
// NORCA_EVENTS_<region> on <SubjectPrefix>.<region>.<namespace>.<topic>.
type EventsConfig struct {
	Enabled       bool
	SubjectPrefix string
	MaxAge        time.Duration
	MaxBytes      int64

	// MaxConsumers bounds the consumers of each change data capture stream.
	MaxConsumers int

	// Topics are the published topics; "*" publishes all.
	Topics []string
}

// DefaultConfig returns the defaults of the system bus. It is disabled: the
// agent enables it on servers.
func DefaultConfig() *Config {
	return &Config{
		Port:        DefaultPort,
		ClusterPort: DefaultClusterPort,
		LeafNodes:   LeafNodesConfig{Port: DefaultLeafNodePort},
		MaxMemory:   DefaultMaxMemory,
		MaxStorage:  DefaultMaxStorage,

		ConsulKVMaxBytes: DefaultConsulKVMaxBytes,
		MaxPayload:       DefaultMaxPayload,
		MaxConnections:   DefaultMaxConnections,

		ReaderCompactThreshold: DefaultReaderCompactThreshold,
		Events: EventsConfig{
			Enabled:       true,
			SubjectPrefix: DefaultSubjectPrefix,
			MaxAge:        DefaultEventsMaxAge,
			MaxBytes:      DefaultEventsMaxBytes,
			MaxConsumers:  DefaultEventsMaxConsumers,
			Topics:        []string{"*"},
		},
	}
}

// Copy returns a copy of the configuration; the TLS configurations are
// shared.
func (c *Config) Copy() *Config {
	if c == nil {
		return nil
	}
	nc := *c
	nc.Events.Topics = append([]string(nil), c.Events.Topics...)
	return &nc
}

// Validate checks the limits and ports of an enabled bus.
func (c *Config) Validate() error {
	if c == nil || !c.Enabled {
		return nil
	}
	var errs []error
	if c.Port <= 0 || c.Port > 65535 {
		errs = append(errs, fmt.Errorf("invalid bus port %d", c.Port))
	}
	if c.ClusterPort <= 0 || c.ClusterPort > 65535 {
		errs = append(errs, fmt.Errorf("invalid bus_cluster port %d", c.ClusterPort))
	}
	if c.Port == c.ClusterPort {
		errs = append(errs, fmt.Errorf("the bus and bus_cluster ports must differ"))
	}
	if c.LeafNodes.Enabled {
		if c.LeafNodes.Port <= 0 || c.LeafNodes.Port > 65535 {
			errs = append(errs, fmt.Errorf("invalid bus_leafnode port %d", c.LeafNodes.Port))
		}
		if c.LeafNodes.Port == c.Port || c.LeafNodes.Port == c.ClusterPort {
			errs = append(errs, fmt.Errorf("the bus_leafnode port must differ from the bus and bus_cluster ports"))
		}
	}
	if c.MaxMemory <= 0 {
		errs = append(errs, fmt.Errorf("bus.max_memory must be positive"))
	}
	if c.MaxStorage <= 0 {
		errs = append(errs, fmt.Errorf("bus.max_storage must be positive"))
	}
	if c.MaxPayload <= 0 {
		errs = append(errs, fmt.Errorf("bus.max_payload must be positive"))
	}
	if c.ConsulKVMaxBytes <= 0 {
		errs = append(errs, fmt.Errorf("bus.consul_kv_max_bytes must be positive"))
	}
	if c.MaxConnections <= 0 {
		errs = append(errs, fmt.Errorf("bus.max_connections must be positive"))
	}
	if c.ReaderCompactThreshold <= 0 {
		errs = append(errs, fmt.Errorf("bus.reader_compact_threshold must be positive"))
	}
	if c.Events.Enabled {
		if err := ValidSubjectPrefix(c.Events.SubjectPrefix); err != nil {
			errs = append(errs, fmt.Errorf("bus.events.subject_prefix: %w", err))
		}
		if c.Events.MaxAge < 0 || c.Events.MaxBytes < 0 {
			errs = append(errs, fmt.Errorf("bus.events limits must not be negative"))
		}
		if c.Events.MaxConsumers <= 0 {
			errs = append(errs, fmt.Errorf("bus.events.max_consumers must be positive"))
		}
		// JetStream reserves the limits of the streams: the event and push
		// streams must leave room for the streams without a limit (the
		// Consul KV bucket).
		if len(c.Events.Topics) == 0 {
			errs = append(errs, fmt.Errorf("bus.events.topics must name at least one topic or \"*\""))
		}
	}
	// Everything the bus keeps on disk fits into its storage: a stream that
	// may grow past what is left makes the writes of the others fail.
	if c.MaxStorage > 0 && c.ConsulKVMaxBytes > 0 {
		if planned := c.PlannedStorage(); planned > c.MaxStorage {
			errs = append(errs, fmt.Errorf("the streams of the bus need %dMiB (events %dMiB, "+
				"usage reports %dMiB, Consul KV %dMiB, Rigger logs %dMiB, Rigger bundles %dMiB) and bus.max_storage is %dMiB: "+
				"raise it, or lower bus.events.max_bytes, bus.consul_kv_max_bytes, bus.rigger_logs_max_bytes or bus.rigger_bundles_max_bytes",
				planned>>20, c.eventsStorage()>>20, c.TelemetryMaxBytes()>>20,
				c.ConsulKVMaxBytes>>20, c.RiggerLogsBytes()>>20, c.RiggerBundlesBytes()>>20, c.MaxStorage>>20))
		}
	}
	return errors.Join(errs...)
}

// TelemetryMaxBytes is the size limit of the stream of the usage reports.
func (c *Config) TelemetryMaxBytes() int64 {
	return c.MaxStorage / TelemetryStorageShare
}

// EventStreamMaxBytes, AdminEventStreamMaxBytes and
// ConsulKVChangesMaxBytes are the size limits of the event stream, the
// admin event stream and the stream of the Consul KV notices: they share
// bus.events.max_bytes, the admin events a part of AdminEventsShare, the
// notices a part of ConsulKVChangesShare. 0 is no limit for all.
func (c *Config) EventStreamMaxBytes() int64 {
	return c.Events.MaxBytes - c.AdminEventStreamMaxBytes() - c.ConsulKVChangesMaxBytes()
}

func (c *Config) ConsulKVChangesMaxBytes() int64 {
	return c.Events.MaxBytes / ConsulKVChangesShare
}

func (c *Config) AdminEventStreamMaxBytes() int64 {
	return c.Events.MaxBytes / AdminEventsShare
}

func (c *Config) eventsStorage() int64 {
	if !c.Events.Enabled {
		return 0
	}
	return c.Events.MaxBytes
}

// PlannedStorage is what the streams of the bus may take on disk together.
func (c *Config) PlannedStorage() int64 {
	return c.streamsStorage() + c.RiggerBundlesBytes()
}

// streamsStorage is what the streams of the bus but the object store of
// the bundles of Rigger may take on disk together.
func (c *Config) streamsStorage() int64 {
	return c.eventsStorage() + c.TelemetryMaxBytes() + c.ConsulKVMaxBytes + c.RiggerLogsBytes()
}

// minRiggerBundlesBytes is the least room a default gives the bundles;
// with less there is no object store of the bundles.
const minRiggerBundlesBytes = 1 << 20

// RiggerBundlesBytes is the size limit of the object store of the bundles
// of Rigger: RiggerBundlesMaxBytes, else DefaultRiggerBundlesMaxBytes or
// half of the storage the other streams leave when that is less, so that a
// small bus.max_storage still holds the plan. Zero is no object store: the
// other streams leave no room for one.
func (c *Config) RiggerBundlesBytes() int64 {
	if c.RiggerBundlesMaxBytes > 0 {
		return c.RiggerBundlesMaxBytes
	}

	n := min(DefaultRiggerBundlesMaxBytes, (c.MaxStorage-c.streamsStorage())/2)
	if n < minRiggerBundlesBytes {
		return 0
	}

	return n
}

// RiggerLogsBytes is the size limit of the stream of the logs of the
// operations of Rigger: RiggerLogsMaxBytes, else DefaultRiggerLogsMaxBytes
// or a sixteenth of the storage when that is less.
func (c *Config) RiggerLogsBytes() int64 {
	if c.RiggerLogsMaxBytes > 0 {
		return c.RiggerLogsMaxBytes
	}

	return min(DefaultRiggerLogsMaxBytes, c.MaxStorage/16)
}

// RiggerLogsAge is how long the log of an operation of Rigger is kept:
// RiggerLogsMaxAge, else DefaultRiggerLogsMaxAge.
func (c *Config) RiggerLogsAge() time.Duration {
	if c.RiggerLogsMaxAge > 0 {
		return c.RiggerLogsMaxAge
	}

	return DefaultRiggerLogsMaxAge
}

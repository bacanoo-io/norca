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

	// DefaultMaxMemory and DefaultMaxStorage bound the JetStream memory and
	// file stores of a server.
	DefaultMaxMemory  = 256 << 20
	DefaultMaxStorage = 4 << 30

	// DefaultMaxPayload is the largest message a connection may publish.
	DefaultMaxPayload = 1 << 20

	// DefaultMaxConnections bounds the client connections of a server.
	DefaultMaxConnections = 10000

	// DefaultEventsMaxAge and DefaultEventsMaxBytes bound the event stream.
	DefaultEventsMaxAge   = 72 * time.Hour
	DefaultEventsMaxBytes = 1 << 30

	// PushStreamMaxBytes bounds the stream of the pack push notices, which
	// runs with the events: push notices are small and only needed until
	// the leader handled them.
	PushStreamMaxBytes = 64 << 20
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

	// MaxPayload bounds the size of a message in bytes.
	MaxPayload int32

	// MaxConnections bounds the client connections.
	MaxConnections int

	// ClientTLS secures the client listener and ClusterTLS the routes. Both
	// are nil without TLS. ClusterTLS is used for incoming and outgoing
	// routes, so it must verify the peer itself.
	ClientTLS  *tls.Config
	ClusterTLS *tls.Config

	// Events configures the event stream the leader publishes.
	Events EventsConfig
}

// EventsConfig configures the event stream of the region: the leader
// publishes the events of the event broker into the JetStream stream
// NORCA_EVENTS_<region> on <SubjectPrefix>.<region>.<topic>, and consumes
// the pack push notices on <SubjectPrefix>.packs.pushed.
type EventsConfig struct {
	Enabled       bool
	SubjectPrefix string
	MaxAge        time.Duration
	MaxBytes      int64

	// Topics are the published topics; "*" publishes all.
	Topics []string
}

// DefaultConfig returns the defaults of the system bus. It is disabled: the
// agent enables it on servers.
func DefaultConfig() *Config {
	return &Config{
		Port:           DefaultPort,
		ClusterPort:    DefaultClusterPort,
		MaxMemory:      DefaultMaxMemory,
		MaxStorage:     DefaultMaxStorage,
		MaxPayload:     DefaultMaxPayload,
		MaxConnections: DefaultMaxConnections,
		Events: EventsConfig{
			Enabled:       true,
			SubjectPrefix: DefaultSubjectPrefix,
			MaxAge:        DefaultEventsMaxAge,
			MaxBytes:      DefaultEventsMaxBytes,
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
	if c.MaxMemory <= 0 {
		errs = append(errs, fmt.Errorf("bus.max_memory must be positive"))
	}
	if c.MaxStorage <= 0 {
		errs = append(errs, fmt.Errorf("bus.max_storage must be positive"))
	}
	if c.MaxPayload <= 0 {
		errs = append(errs, fmt.Errorf("bus.max_payload must be positive"))
	}
	if c.MaxConnections <= 0 {
		errs = append(errs, fmt.Errorf("bus.max_connections must be positive"))
	}
	if c.Events.Enabled {
		if err := ValidSubjectPrefix(c.Events.SubjectPrefix); err != nil {
			errs = append(errs, fmt.Errorf("bus.events.subject_prefix: %w", err))
		}
		if c.Events.MaxAge < 0 || c.Events.MaxBytes < 0 {
			errs = append(errs, fmt.Errorf("bus.events limits must not be negative"))
		}
		// JetStream reserves the limits of the streams: the event and push
		// streams must leave room for the streams without a limit (the
		// Consul KV bucket).
		if c.Events.MaxBytes+PushStreamMaxBytes >= c.MaxStorage {
			errs = append(errs, fmt.Errorf("bus.events.max_bytes and the %dMiB of the push notices must fit into bus.max_storage", PushStreamMaxBytes>>20))
		}
		if len(c.Events.Topics) == 0 {
			errs = append(errs, fmt.Errorf("bus.events.topics must name at least one topic or \"*\""))
		}
	}
	return errors.Join(errs...)
}

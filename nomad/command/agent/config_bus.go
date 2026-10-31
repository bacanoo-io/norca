// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/bus"
)

// BusConfig is the server's bus block: the system bus, a NATS server with
// JetStream embedded in the servers. It listens on ports.bus (clients) and
// ports.bus_cluster (routes between the servers of a region), with the
// agent's RPC TLS configuration.
//
//	bus {
//	  enabled         = true
//	  max_memory      = "256MiB"
//	  max_storage     = "4GiB"
//	  max_payload     = "1MiB"
//	  max_connections = 10000
//
//	  events {
//	    enabled        = true
//	    subject_prefix = "nomad"
//	    max_age        = "72h"
//	    max_bytes      = "1GiB"
//	    topics         = ["*"]
//	  }
//	}
type BusConfig struct {
	// Enabled starts the system bus; true by default.
	Enabled *bool `hcl:"enabled"`

	// MaxMemory and MaxStorage bound the JetStream memory and file stores.
	MaxMemory  string `hcl:"max_memory"`
	MaxStorage string `hcl:"max_storage"`

	// MaxPayload bounds the size of a message.
	MaxPayload string `hcl:"max_payload"`

	// MaxConnections bounds the client connections of a server.
	MaxConnections int `hcl:"max_connections"`

	// Events configures the event stream the leader publishes.
	Events *BusEventsConfig `hcl:"events"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// BusEventsConfig is the events block of the bus: the leader publishes the
// events of the region into the stream NORCA_EVENTS_<region>.
type BusEventsConfig struct {
	Enabled       *bool    `hcl:"enabled"`
	SubjectPrefix string   `hcl:"subject_prefix"`
	MaxAge        string   `hcl:"max_age"`
	MaxBytes      string   `hcl:"max_bytes"`
	Topics        []string `hcl:"topics"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

func (e *BusEventsConfig) Copy() *BusEventsConfig {
	if e == nil {
		return nil
	}
	ne := *e
	ne.Enabled = pointer.Copy(e.Enabled)
	ne.Topics = slices.Clone(e.Topics)
	ne.ExtraKeysHCL = slices.Clone(e.ExtraKeysHCL)
	return &ne
}

// Merge merges b into a copy of a.
func (a *BusEventsConfig) Merge(b *BusEventsConfig) *BusEventsConfig {
	if a == nil {
		return b.Copy()
	}
	result := a.Copy()
	if b == nil {
		return result
	}
	if b.Enabled != nil {
		result.Enabled = pointer.Copy(b.Enabled)
	}
	if b.SubjectPrefix != "" {
		result.SubjectPrefix = b.SubjectPrefix
	}
	if b.MaxAge != "" {
		result.MaxAge = b.MaxAge
	}
	if b.MaxBytes != "" {
		result.MaxBytes = b.MaxBytes
	}
	if b.Topics != nil {
		result.Topics = slices.Clone(b.Topics)
	}
	return result
}

// extraKeys checks the events block, which UnusedKeys does not descend
// into.
func (b *BusConfig) extraKeys() error {
	if b == nil || b.Events == nil {
		return nil
	}
	helper.RemoveEqualFold(&b.ExtraKeysHCL, "events")
	if err := helper.UnusedKeys(b.Events); err != nil {
		return fmt.Errorf("server.bus.events: %w", err)
	}
	return nil
}

func (b *BusConfig) Copy() *BusConfig {
	if b == nil {
		return nil
	}
	nb := *b
	nb.Enabled = pointer.Copy(b.Enabled)
	nb.Events = b.Events.Copy()
	nb.ExtraKeysHCL = slices.Clone(b.ExtraKeysHCL)
	return &nb
}

// Merge merges b into a copy of a.
func (a *BusConfig) Merge(b *BusConfig) *BusConfig {
	if a == nil {
		return b.Copy()
	}
	result := a.Copy()
	if b == nil {
		return result
	}
	if b.Enabled != nil {
		result.Enabled = pointer.Copy(b.Enabled)
	}
	if b.MaxMemory != "" {
		result.MaxMemory = b.MaxMemory
	}
	if b.MaxStorage != "" {
		result.MaxStorage = b.MaxStorage
	}
	if b.MaxPayload != "" {
		result.MaxPayload = b.MaxPayload
	}
	if b.MaxConnections != 0 {
		result.MaxConnections = b.MaxConnections
	}
	if b.Events != nil {
		result.Events = result.Events.Merge(b.Events)
	}
	return result
}

// toServerConfig converts the agent configuration into the server's bus
// configuration, applying defaults and validating it. The ports come from
// the agent's ports block.
func (b *BusConfig) toServerConfig(ports *Ports) (*bus.Config, error) {
	cfg := bus.DefaultConfig()
	cfg.Enabled = true
	if ports != nil {
		if ports.Bus != 0 {
			cfg.Port = ports.Bus
		}
		if ports.BusCluster != 0 {
			cfg.ClusterPort = ports.BusCluster
		}
	}
	if b == nil {
		b = &BusConfig{}
	}

	if b.Enabled != nil {
		cfg.Enabled = *b.Enabled
	}
	sizes := []struct {
		name  string
		value string
		field *int64
	}{
		{"max_memory", b.MaxMemory, &cfg.MaxMemory},
		{"max_storage", b.MaxStorage, &cfg.MaxStorage},
	}
	for _, s := range sizes {
		if s.value == "" {
			continue
		}
		v, err := humanize.ParseBytes(s.value)
		if err != nil || v > math.MaxInt64 {
			return nil, fmt.Errorf("bus.%s can't parse size %s", s.name, s.value)
		}
		*s.field = int64(v)
	}
	if b.MaxPayload != "" {
		v, err := humanize.ParseBytes(b.MaxPayload)
		if err != nil || v > 64<<20 {
			return nil, fmt.Errorf("bus.max_payload must be a size up to 64MiB, not %s", b.MaxPayload)
		}
		cfg.MaxPayload = int32(v)
	}
	if b.MaxConnections != 0 {
		cfg.MaxConnections = b.MaxConnections
	}

	// The event stream takes at most a quarter of the storage unless its
	// limit is set.
	cfg.Events.MaxBytes = min(cfg.Events.MaxBytes, cfg.MaxStorage/4)
	if e := b.Events; e != nil {
		if e.Enabled != nil {
			cfg.Events.Enabled = *e.Enabled
		}
		if e.SubjectPrefix != "" {
			cfg.Events.SubjectPrefix = e.SubjectPrefix
		}
		if e.MaxAge != "" {
			d, err := time.ParseDuration(e.MaxAge)
			if err != nil {
				return nil, fmt.Errorf("bus.events.max_age can't parse time duration %s", e.MaxAge)
			}
			cfg.Events.MaxAge = d
		}
		if e.MaxBytes != "" {
			v, err := humanize.ParseBytes(e.MaxBytes)
			if err != nil || v > math.MaxInt64 {
				return nil, fmt.Errorf("bus.events.max_bytes can't parse size %s", e.MaxBytes)
			}
			cfg.Events.MaxBytes = int64(v)
		}
		if e.Topics != nil {
			cfg.Events.Topics = slices.Clone(e.Topics)
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ClientUsageConfig is the client's usage block: whether the client reports
// the usage of its task groups (for the built-in autoscaler and the metrics
// store of the servers) when the servers ask for it.
//
//	usage {
//	  enabled = true
//	}
type ClientUsageConfig struct {
	Enabled *bool `hcl:"enabled"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// IsEnabled reports whether the client reports usage; true by default.
func (c *ClientUsageConfig) IsEnabled() bool {
	return c == nil || c.Enabled == nil || *c.Enabled
}

func (c *ClientUsageConfig) Copy() *ClientUsageConfig {
	if c == nil {
		return nil
	}
	nc := *c
	nc.Enabled = pointer.Copy(c.Enabled)
	nc.ExtraKeysHCL = slices.Clone(c.ExtraKeysHCL)
	return &nc
}

// Merge merges b into a copy of a.
func (a *ClientUsageConfig) Merge(b *ClientUsageConfig) *ClientUsageConfig {
	if a == nil {
		return b.Copy()
	}
	result := a.Copy()
	if b != nil && b.Enabled != nil {
		result.Enabled = pointer.Copy(b.Enabled)
	}
	return result
}

// ClientBusConfig is the client's bus block: whether the client connects to
// the system bus of the servers (with its node secret) for the features
// that use it, such as the Consul KV of consul_compat.
//
//	bus {
//	  enabled = true
//	}
type ClientBusConfig struct {
	Enabled *bool `hcl:"enabled"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// IsEnabled reports whether the client uses the bus; true by default.
func (c *ClientBusConfig) IsEnabled() bool {
	return c == nil || c.Enabled == nil || *c.Enabled
}

func (c *ClientBusConfig) Copy() *ClientBusConfig {
	if c == nil {
		return nil
	}
	nc := *c
	nc.Enabled = pointer.Copy(c.Enabled)
	nc.ExtraKeysHCL = slices.Clone(c.ExtraKeysHCL)
	return &nc
}

// Merge merges b into a copy of a.
func (a *ClientBusConfig) Merge(b *ClientBusConfig) *ClientBusConfig {
	if a == nil {
		return b.Copy()
	}
	result := a.Copy()
	if b != nil && b.Enabled != nil {
		result.Enabled = pointer.Copy(b.Enabled)
	}
	return result
}

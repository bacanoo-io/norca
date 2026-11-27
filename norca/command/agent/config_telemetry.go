// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"slices"
	"time"

	"github.com/bacanoo-io/norca/norca/helper"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
)

// removeJSONKeys removes the names of the nested telemetry blocks from the
// unused keys of the enclosing configuration. The JSON reader of hcl takes
// an object whose only member is another object for a block with two
// labels, and leaves the inner name behind as an unused key.
func (t *Telemetry) removeJSONKeys(top *[]string) {
	if t == nil {
		return
	}
	if t.Prometheus != nil {
		helper.RemoveEqualFold(top, "prometheus")
	}
}

// DefaultPrometheusExpiration is how long a metric stays in the Prometheus
// output after its last update when prometheus.expiration is not set.
const DefaultPrometheusExpiration = 60 * time.Second

// PrometheusConfig is the prometheus block of telemetry:
//
//	prometheus {
//	  enabled    = true
//	  expiration = "60s"
//	}
type PrometheusConfig struct {
	// Enabled turns the Prometheus sink on. Off by default, on in -dev.
	Enabled *bool `hcl:"enabled"`

	// Expiration is how long a metric stays in the output after its last
	// update (default 60s); "0s" keeps every metric forever.
	Expiration string `hcl:"expiration"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// IsEnabled reports whether the block turns the sink on.
func (p *PrometheusConfig) IsEnabled() bool {
	return p != nil && p.Enabled != nil && *p.Enabled
}

// ExpirationDuration parses Expiration, defaulting to
// DefaultPrometheusExpiration when it is not set.
func (p *PrometheusConfig) ExpirationDuration() (time.Duration, error) {
	if p == nil || p.Expiration == "" {
		return DefaultPrometheusExpiration, nil
	}
	d, err := time.ParseDuration(p.Expiration)
	if err != nil {
		return 0, fmt.Errorf("telemetry.prometheus.expiration can't parse time duration %s", p.Expiration)
	}
	if d < 0 {
		return 0, fmt.Errorf("telemetry.prometheus.expiration must not be negative: %s", p.Expiration)
	}
	return d, nil
}

// Copy returns a deep copy.
func (p *PrometheusConfig) Copy() *PrometheusConfig {
	if p == nil {
		return nil
	}
	np := *p
	np.Enabled = pointer.Copy(p.Enabled)
	np.ExtraKeysHCL = slices.Clone(p.ExtraKeysHCL)
	return &np
}

// Merge merges b into a copy of a, field by field.
func (p *PrometheusConfig) Merge(b *PrometheusConfig) *PrometheusConfig {
	if p == nil {
		return b.Copy()
	}
	result := p.Copy()
	if b == nil {
		return result
	}
	if b.Enabled != nil {
		result.Enabled = pointer.Copy(b.Enabled)
	}
	if b.Expiration != "" {
		result.Expiration = b.Expiration
	}
	return result
}

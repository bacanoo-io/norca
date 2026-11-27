// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/helper/telemetry"
)

// removeJSONKeys removes the names of the nested telemetry blocks from the
// unused keys of the levels above them. When the blocks are written as JSON
// objects rather than lists of objects, the JSON reader of hcl leaves the
// name of a nested object (a block or a map), and of the blocks between, as
// unused keys of the enclosing levels, once per nested object.
func (t *Telemetry) removeJSONKeys(top *[]string) {
	if t == nil {
		return
	}
	if t.Prometheus != nil {
		removeAllEqualFold(top, "prometheus")
	}
	o := t.OTLP
	if o == nil {
		return
	}
	names := []string{"otlp"}
	targets := []telemetry.TargetConfig{o.TargetConfig}
	if o.Retry != nil {
		names = append(names, "retry")
	}
	if o.ResourceAttributes != nil {
		names = append(names, "resource_attributes")
	}
	if o.Metrics != nil {
		names = append(names, "metrics")
		targets = append(targets, o.Metrics.TargetConfig)
	}
	if o.Traces != nil {
		names = append(names, "traces")
		targets = append(targets, o.Traces.TargetConfig)
	}
	if o.Logs != nil {
		names = append(names, "logs")
		targets = append(targets, o.Logs.TargetConfig)
	}
	if slices.ContainsFunc(targets, func(tc telemetry.TargetConfig) bool { return tc.TLS != nil }) {
		names = append(names, "tls")
	}
	if slices.ContainsFunc(targets, func(tc telemetry.TargetConfig) bool { return tc.Headers != nil }) {
		names = append(names, "headers")
	}
	for _, keys := range []*[]string{top, &t.ExtraKeysHCL, &o.ExtraKeysHCL} {
		for _, name := range names {
			removeAllEqualFold(keys, name)
		}
	}
}

// removeAllEqualFold removes every occurrence of name from keys.
func removeAllEqualFold(keys *[]string, name string) {
	*keys = slices.DeleteFunc(*keys, func(k string) bool { return strings.EqualFold(k, name) })
	if len(*keys) == 0 {
		*keys = nil
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

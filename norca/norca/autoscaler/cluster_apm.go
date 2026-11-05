// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/autoscaler/plugins"
	"github.com/bacanoo-io/norca/norca/norca/autoscaler/sdk"
	"github.com/bacanoo-io/norca/norca/norca/metricsstore"
)

// Config keys of a target that scopes a cluster check. A policy that scales
// the nodes of a pool in a datacenter names them, so its checks need not.
const (
	TargetConfigKeyPool       = "Pool"
	TargetConfigKeyDatacenter = "Datacenter"
	TargetConfigKeyNodeClass  = "Class"
)

// clusterQueryWindow is the range read for an instant query: the cluster
// gauges are sampled every interval of the metrics store, so a shorter range
// can be empty.
const clusterQueryWindow = 2 * time.Minute

// clusterAPM is the built-in source "norca" of the capacity of the cluster:
// the gauges the servers sample from their state, norca.cluster.cpu_allocated
// and the others, per datacenter, node pool and node class. A check names the
// gauge without the prefix and may select the series:
//
//	query = "cluster.cpu_allocated"
//	query = "cluster.nodes_ready?dc=dc-a&pool=workers&class=gpu&op=max"
//
// dc, pool and class default to the target of the policy (Datacenter, Pool
// and Class), and an empty class is the series of all the nodes of the
// datacenter and pool. The value is the mean of the sample intervals in the
// range, or their largest maximum with op=max.
type clusterAPM struct {
	// target is the scope of the policy's target; explicit selectors in the
	// query win over it.
	target map[string]string

	// history reads a gauge over a range (Deps.GaugeHistory).
	history func(metric string, labels map[string]string, from, to time.Time) (avg, max float64, samples int, err error)
}

var _ plugins.Looker = (*clusterAPM)(nil)

// clusterQuery is a parsed query of the source.
type clusterQuery struct {
	metric string
	labels map[string]string
	max    bool
}

func (c *clusterAPM) parseQuery(q string) (*clusterQuery, error) {
	name, rawSelectors, _ := strings.Cut(strings.TrimSpace(q), "?")
	name = strings.TrimPrefix(name, "norca.")
	metric := "norca." + name
	if !strings.HasPrefix(name, "cluster.") {
		return nil, fmt.Errorf("%q is not a cluster metric (use cluster.<name>, for example cluster.cpu_allocated)", q)
	}
	names, ok := metricsstore.GaugeLabels(metric)
	if !ok {
		return nil, fmt.Errorf("unknown cluster metric %q", metric)
	}
	selectors, err := url.ParseQuery(rawSelectors)
	if err != nil {
		return nil, fmt.Errorf("invalid selectors in %q: %w", q, err)
	}

	res := &clusterQuery{metric: metric, labels: map[string]string{}}
	defaults := map[string]string{
		"dc":    c.target[TargetConfigKeyDatacenter],
		"pool":  c.target[TargetConfigKeyPool],
		"class": c.target[TargetConfigKeyNodeClass],
	}
	for _, label := range names {
		res.labels[label] = defaults[label]
		if v, ok := selectors[label]; ok {
			res.labels[label] = v[0]
		}
		delete(selectors, label)
	}
	switch op := selectors.Get("op"); op {
	case "", "avg":
	case "max":
		res.max = true
	default:
		return nil, fmt.Errorf("unknown op %q in %q (use avg or max)", op, q)
	}
	selectors.Del("op")
	if len(selectors) > 0 {
		for k := range selectors {
			return nil, fmt.Errorf("unknown selector %q in %q (use dc, pool, class or op)", k, q)
		}
	}
	return res, nil
}

// Query returns one data point: the value of the gauge over the range.
func (c *clusterAPM) Query(q string, r sdk.TimeRange) (sdk.TimestampedMetrics, error) {
	query, err := c.parseQuery(q)
	if err != nil {
		return nil, err
	}
	if c.history == nil {
		return nil, fmt.Errorf("%s: the metrics store is not available", q)
	}
	from, to := r.From, r.To
	if to.IsZero() || !to.After(from) || to.Sub(from) < clusterQueryWindow {
		// An instant query reads the latest intervals.
		from = to.Add(-clusterQueryWindow)
	}
	avg, max, samples, err := c.history(query.metric, query.labels, from, to)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", q, err)
	}
	if samples == 0 {
		return nil, fmt.Errorf("%s: no samples for %s (is the metrics store enabled, and is there a ready node there?)",
			q, describeSeries(query))
	}
	value := avg
	if query.max {
		value = max
	}
	return sdk.TimestampedMetrics{{Timestamp: r.To, Value: value}}, nil
}

func (c *clusterAPM) QueryMultiple(q string, r sdk.TimeRange) ([]sdk.TimestampedMetrics, error) {
	m, err := c.Query(q, r)
	if err != nil {
		return nil, err
	}
	return []sdk.TimestampedMetrics{m}, nil
}

func describeSeries(q *clusterQuery) string {
	return fmt.Sprintf("%s dc=%q pool=%q class=%q", q.metric, q.labels["dc"], q.labels["pool"], q.labels["class"])
}

// clusterLooker returns the cluster source bound to the scope of the policy's
// target.
func (r *runner) clusterLooker(policy *sdk.ScalingPolicy) plugins.Looker {
	scope := map[string]string{}
	if policy.Target != nil {
		for _, key := range []string{TargetConfigKeyPool, TargetConfigKeyDatacenter, TargetConfigKeyNodeClass} {
			scope[key] = policy.Target.Config[key]
		}
	}
	return &clusterAPM{target: scope, history: r.deps.GaugeHistory}
}

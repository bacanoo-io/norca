// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/nomad/nomad/autoscaler/plugins"
	"github.com/hashicorp/nomad/nomad/autoscaler/sdk"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// Operations and metrics of the nomad-apm query form
// (taskgroup_<op>_<metric>) that the usage collector can serve.
const (
	usageQueryPrefix = "taskgroup_"

	usageOpAvg = "avg"
	usageOpMax = "max"
	usageOpSum = "sum"

	legacyMetricCPUAllocated = "cpu-allocated"
	legacyMetricMemAllocated = "memory-allocated"
	legacyMetricMem          = "memory"
)

// usageAPM is the built-in source of the Nomad metrics. It serves the checks
// whose source is the nomad-apm from the leader's usage collector, so the
// evaluation makes no ClientAllocations.Stats call. It understands the
// built-in metric names (nomad.cpu, nomad.memory, nomad.cpu_mhz,
// nomad.memory_mb, which use the average) and the nomad-apm query form
// taskgroup_<avg|max|sum>_<cpu-allocated|memory-allocated|memory> of the
// external autoscaler.
//
// A usageAPM is bound to the task group of its policy, which is used when
// the query does not name one.
type usageAPM struct {
	collector *usageCollector
	state     func() *state.StateStore
	target    targetKey
	now       func() time.Time

	// history reads the metrics store (Deps.UsageHistory); nil without one.
	history func(namespace, job, group, metric string, from, to time.Time) (avg, max float64, samples int, err error)
}

// historyMetrics are the names of the collected values in the metrics
// store.
var historyMetrics = map[usageMetric]string{
	usageCPUPercent:    "norca.group.cpu",
	usageMemoryPercent: "norca.group.memory",
	usageCPUMHz:        "norca.group.cpu_mhz",
	usageMemoryMB:      "norca.group.memory_mb",
}

// historyInstantRange is the range of the metrics store read for an
// instant query: the latest sample intervals.
const historyInstantRange = time.Minute

var _ plugins.Looker = (*usageAPM)(nil)

// usageQuery is a parsed usageAPM query.
type usageQuery struct {
	key    targetKey
	op     string
	metric usageMetric

	// scale converts the collected value into the unit of the query.
	scale float64
}

// Query returns one data point: the aggregate of the usage reported within
// the time range.
func (u *usageAPM) Query(q string, r sdk.TimeRange) (sdk.TimestampedMetrics, error) {
	query, err := u.parseQuery(q)
	if err != nil {
		return nil, err
	}

	agg, err := u.aggregate(query, r)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", q, err)
	}

	var value float64
	switch query.op {
	case usageOpMax:
		value = agg.max
	case usageOpSum:
		n, err := u.runningAllocs(query.key)
		if err != nil {
			return nil, err
		}
		value = agg.avg * float64(n)
	default:
		value = agg.avg
	}

	return sdk.TimestampedMetrics{{Timestamp: r.To, Value: value * query.scale}}, nil
}

// aggregate returns the usage of the query's task group in the range: from
// the metrics store when the range reaches back beyond the collector's
// window (metrics_window) or the collector has no full window yet (after a
// leader change), otherwise, and when the store has no points, from the
// collector, which limits the range to its window.
func (u *usageAPM) aggregate(query *usageQuery, r sdk.TimeRange) (usageAggregate, error) {
	now := u.now()
	if u.history != nil && r.From.Before(now.Add(-u.collector.windowSize())) {
		if agg, err := u.fromHistory(query, r); !errors.Is(err, errNoUsage) {
			return agg, err
		}
	}
	agg, err := u.collector.query(now, query.key, query.metric, r.From, r.To)
	if errors.Is(err, ErrInsufficientData) && u.history != nil {
		if hist, herr := u.fromHistory(query, r); herr == nil {
			return hist, nil
		}
	}
	return agg, err
}

// fromHistory reads the usage from the metrics store.
func (u *usageAPM) fromHistory(query *usageQuery, r sdk.TimeRange) (usageAggregate, error) {
	from := r.From
	if !from.Before(r.To) {
		from = r.To.Add(-historyInstantRange)
	}
	avg, mx, samples, err := u.history(query.key.namespace, query.key.job, query.key.group,
		historyMetrics[query.metric], from, r.To)
	if err != nil {
		return usageAggregate{}, err
	}
	if samples == 0 {
		return usageAggregate{}, errNoUsage
	}
	return usageAggregate{avg: avg, max: mx}, nil
}

// QueryMultiple returns the single stream of Query.
func (u *usageAPM) QueryMultiple(q string, r sdk.TimeRange) ([]sdk.TimestampedMetrics, error) {
	m, err := u.Query(q, r)
	if err != nil {
		return nil, err
	}
	return []sdk.TimestampedMetrics{m}, nil
}

// parseQuery parses a built-in metric name or a nomad-apm query, with or
// without the /<group>/<job>@<namespace> suffix of the canonical form.
func (u *usageAPM) parseQuery(q string) (*usageQuery, error) {
	query := &usageQuery{key: u.target, op: usageOpAvg, scale: 1}

	expr := q
	if parts := strings.SplitN(q, "/", 3); len(parts) == 3 {
		idx := strings.LastIndex(parts[2], "@")
		if idx <= 0 || idx == len(parts[2])-1 || parts[1] == "" {
			return nil, fmt.Errorf("expected <query>/<group>/<job>@<namespace>, received %s", q)
		}
		expr = parts[0]
		query.key = targetKey{namespace: parts[2][idx+1:], job: parts[2][:idx], group: parts[1]}
	} else if strings.Contains(q, "/") {
		return nil, fmt.Errorf("expected <query>/<group>/<job>@<namespace>, received %s", q)
	}
	expr = strings.TrimPrefix(expr, usageQueryPrefix)

	if m, ok := builtinMetrics[expr]; ok {
		query.metric = m
		return query, nil
	}

	op, metric, ok := strings.Cut(expr, "_")
	if !ok {
		return nil, fmt.Errorf("unsupported metric %q, allowed are %s, %s, %s, %s or taskgroup_<op>_<metric>",
			expr, MetricCPU, MetricMemory, MetricCPUMHz, MetricMemoryMB)
	}

	switch op {
	case usageOpAvg, usageOpMax, usageOpSum:
		query.op = op
	default:
		return nil, fmt.Errorf("invalid operation %q, allowed values are %s, %s or %s",
			op, usageOpAvg, usageOpMax, usageOpSum)
	}

	switch metric {
	case legacyMetricCPUAllocated:
		query.metric = usageCPUPercent
	case legacyMetricMemAllocated:
		query.metric = usageMemoryPercent
	case legacyMetricMem:
		// The nomad-apm reports memory in bytes.
		query.metric = usageMemoryMB
		query.scale = 1024 * 1024
	default:
		return nil, fmt.Errorf("invalid metric %q, allowed values are %s, %s or %s",
			metric, legacyMetricCPUAllocated, legacyMetricMemAllocated, legacyMetricMem)
	}
	return query, nil
}

// runningAllocs counts the running allocations of a task group.
func (u *usageAPM) runningAllocs(key targetKey) (int, error) {
	store := u.state()
	if store == nil {
		return 0, fmt.Errorf("state store not available")
	}
	allocs, err := store.AllocsByJob(nil, key.namespace, key.job, false)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range allocs {
		if a.TaskGroup == key.group && a.ClientStatus == structs.AllocClientStatusRunning {
			n++
		}
	}
	return n, nil
}

// usageLooker returns the usage APM bound to the policy's task group. It is
// used instead of the plugin host's nomad-apm for checks whose source is a
// nomad-apm, so evaluations read the client pushed usage.
func (r *runner) usageLooker(policy *sdk.ScalingPolicy) plugins.Looker {
	key := targetKey{}
	if policy.Target != nil {
		key = targetKey{
			namespace: policy.Target.Config[sdk.TargetConfigKeyNamespace],
			job:       policy.Target.Config[sdk.TargetConfigKeyJob],
			group:     policy.Target.Config[sdk.TargetConfigKeyTaskGroup],
		}
	}
	if key.namespace == "" {
		key.namespace = structs.DefaultNamespace
	}
	return &usageAPM{collector: r.usage, state: r.deps.State, target: key, now: r.now, history: r.deps.UsageHistory}
}

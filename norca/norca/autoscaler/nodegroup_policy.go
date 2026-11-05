// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	memdb "github.com/hashicorp/go-memdb"
)

// Node groups: every enabled NodeGroup object whose engine is the built-in
// one is turned into a scaling policy that the autoscaler evaluates like the
// policy of a task group. The policy is not stored: it is derived from the
// object every time the objects change. The checks, strategies, cooldown, rate
// limit and oscillation guard are the ones task groups use; the target is the
// object itself (its scale subresource, spec.replicas), and the cooldown is
// read from its event log. With enforce = false the decisions are dry runs:
// events only, spec.replicas is not changed.

const (
	// nodeGroupPolicyPrefix starts the ID of a derived policy.
	nodeGroupPolicyPrefix = "nodegroup:"

	// targetKindNodeGroup marks the targetKey of a node group.
	targetKindNodeGroup = "nodegroup"
)

var nodeDurationSegment = regexp.MustCompile(`([0-9]+)(s|m|h|d)`)

// parseNodeDuration parses a duration of the NodeGroup schema: segments of
// seconds, minutes, hours and days ("45d", "1h30m").
func parseNodeDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	if rest := nodeDurationSegment.ReplaceAllString(s, ""); rest != "" {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	units := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}
	var total time.Duration
	for _, m := range nodeDurationSegment.FindAllStringSubmatch(s, -1) {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		total += time.Duration(n) * units[m[2]]
	}
	return total, nil
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asInt(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case uint64:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

// block wraps content as the document of a labeled HCL block: a list with one
// map from the label to a list with the content.
func block(label string, content map[string]any) []any {
	return []any{map[string]any{label: []any{content}}}
}

// durationDoc returns the duration of the field as a string the policy
// parser reads, or "" when it is not set.
func durationDoc(m map[string]any, key string) (string, error) {
	s := asString(m[key])
	if s == "" {
		return "", nil
	}
	d, err := parseNodeDuration(s)
	if err != nil {
		return "", fmt.Errorf("%s: %w", key, err)
	}
	return d.String(), nil
}

// nodeGroupPolicy derives the scaling policy of a NodeGroup object. A group
// that is disabled, or that belongs to the external autoscaler, gives a
// disabled policy, so the caller decides what to do with it.
func nodeGroupPolicy(obj *structs.CustomObject) (*structs.ScalingPolicy, error) {
	spec := obj.Spec
	p := &structs.ScalingPolicy{
		ID:          nodeGroupPolicyPrefix + obj.Name,
		Type:        structs.ScalingPolicyTypeHorizontal,
		Min:         asInt(spec["min"]),
		Max:         asInt(spec["max"]),
		Engine:      structs.ScalingPolicyEngineBuiltin,
		Enabled:     true,
		CreateIndex: obj.CreateIndex,
		ModifyIndex: obj.ModifyIndex,
		Target: map[string]string{
			structs.ScalingTargetNodeGroup: obj.Name,
			TargetConfigKeyPool:            asString(spec["pool"]),
			TargetConfigKeyDatacenter:      asString(spec["datacenter"]),
		},
	}
	if enabled, ok := spec["enabled"].(bool); ok && !enabled {
		p.Enabled = false
	}
	if engine := asString(spec["engine"]); engine != "" && engine != structs.ScalingPolicyEngineBuiltin {
		p.Engine = engine
		p.Enabled = false
	}
	if class := asString(spec["node_class"]); class != "" {
		p.Target[TargetConfigKeyNodeClass] = class
	}
	if enforce, _ := spec["enforce"].(bool); !enforce {
		p.Target["dry-run"] = "true"
	}

	doc := map[string]any{}
	if pol := asMap(spec["policy"]); pol != nil {
		for _, key := range []string{"evaluation_interval", "cooldown"} {
			s, err := durationDoc(pol, key)
			if err != nil {
				return nil, err
			}
			if s != "" {
				doc[key] = s
			}
		}
		var checks []any
		for name, raw := range asMap(pol["check"]) {
			check := asMap(raw)
			content := map[string]any{
				"source": asString(check["source"]),
				"query":  asString(check["query"]),
			}
			w, err := durationDoc(check, "query_window")
			if err != nil {
				return nil, fmt.Errorf("check %q: %w", name, err)
			}
			if w != "" {
				content["query_window"] = w
			}
			if onError := asString(check["on_error"]); onError != "" {
				content["on_error"] = onError
			}
			for strategy, cfg := range asMap(check["strategy"]) {
				content["strategy"] = block(strategy, asMap(cfg))
			}
			checks = append(checks, map[string]any{name: []any{content}})
		}
		if len(checks) > 0 {
			doc["check"] = checks
		}
	}
	p.Policy = doc
	return p, nil
}

// nodeGroupPolicies returns the policies of the NodeGroup objects of the
// built-in engine, and the index of the table they were read at.
func nodeGroupPolicies(ws memdb.WatchSet, store *state.StateStore) ([]*structs.ScalingPolicy, uint64, error) {
	iter, err := store.CustomObjects(ws, structs.NodeScalingGroup, structs.NodeGroupKind, structs.AllNamespacesSentinel, state.SortDefault)
	if err != nil {
		return nil, 0, err
	}
	var out []*structs.ScalingPolicy
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		obj := raw.(*structs.CustomObject)
		if obj.Deleting() {
			continue
		}
		p, err := nodeGroupPolicy(obj)
		if err != nil {
			// The schema rejects what the conversion can't read, so this
			// is an object stored by another version.
			continue
		}
		out = append(out, p)
	}
	index, err := store.Index(state.TableCustomObjects)
	return out, index, err
}

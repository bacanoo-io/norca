// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The demand binding of node scaling. When the autoscaler raises the scale of
// a node group it writes what the decision was made for to status.decision of
// the group: the blocked evaluations, the work and the reason. The operator
// that creates the nodes reads it, and it stays until the nodes the decision
// asked for are ready (or the scale is set below it), because the events of
// the object log are a ring that a burst can push out. The Scaled event keeps
// a copy for the history.
//
// The status of a group is written by the operator too, and a status write
// replaces the whole status: the decision is written by reading the object,
// changing only status.decision and writing it back with the index it was
// read at. A write that loses the race is tried again at the next look.

const (
	// decisionMaxEvals and decisionMaxWork bound the lists of a decision.
	decisionMaxEvals = 50
	decisionMaxWork  = 20
)

// decisionKey is the field of the status the autoscaler writes.
const decisionKey = "decision"

// decisionDoc builds status.decision for a decision that raised the scale of
// the group from previous to replicas.
func decisionDoc(now time.Time, g *demandGroup, previous, replicas int64, check, reason string) map[string]any {
	var ids []string
	type workKey struct{ ns, job, group string }
	counts := map[workKey]int{}
	var order []workKey
	for _, u := range g.bucket {
		if u.EvalID != "" && !slices.Contains(ids, u.EvalID) {
			ids = append(ids, u.EvalID)
		}
		k := workKey{u.Namespace, u.Job, u.Group}
		if _, ok := counts[k]; !ok {
			order = append(order, k)
		}
		counts[k] += max(u.Count, 1)
	}
	slices.Sort(ids)
	slices.SortFunc(order, func(a, b workKey) int {
		return cmp.Or(cmp.Compare(a.ns, b.ns), cmp.Compare(a.job, b.job), cmp.Compare(a.group, b.group))
	})
	var evals []any
	for _, id := range ids[:min(len(ids), decisionMaxEvals)] {
		evals = append(evals, id)
	}
	var work []any
	for _, k := range order {
		if len(work) == decisionMaxWork {
			break
		}
		work = append(work, map[string]any{
			"namespace": k.ns, "job": k.job, "group": k.group, "count": float64(counts[k]),
		})
	}
	doc := map[string]any{
		"time":     now.UTC().Format(time.RFC3339),
		"replicas": float64(replicas),
		"previous": float64(previous),
		"check":    check,
		"reason":   reason,
	}
	if len(evals) > 0 {
		doc["evaluations"] = evals
	}
	if len(work) > 0 {
		doc["work"] = work
	}
	return doc
}

// recordDecision writes status.decision after the scale of the group was
// raised to replicas. It writes nothing when the scale was not changed (a dry
// run, the rate limit, the oscillation guard) or the decision is already
// there.
func (r *runner) recordDecision(g *demandGroup, replicas int64, check, reason string) {
	obj, err := r.groupObject(g)
	if err != nil || obj == nil {
		return
	}
	if asInt(obj.Spec["replicas"]) != replicas {
		return
	}
	if d := asMap(obj.Status[decisionKey]); d != nil && asInt(d["replicas"]) == replicas {
		return
	}
	doc := decisionDoc(r.now(), g, g.spec, replicas, check, reason)
	if err := r.writeGroupStatus(obj, doc); err != nil {
		r.logger.Warn("failed to write the decision of a node group", "group", g.name, "error", err)
	}
}

// settleDecision removes status.decision when what it asked for is done: the
// group has the nodes it was raised to, or its scale was set below that.
func (r *runner) settleDecision(g *demandGroup) {
	if g.decision == nil {
		return
	}
	want := asInt(g.decision["replicas"])
	if g.ready < want && g.spec >= want {
		return
	}
	obj, err := r.groupObject(g)
	if err != nil || obj == nil {
		return
	}
	if err := r.writeGroupStatus(obj, nil); err != nil {
		r.logger.Warn("failed to clear the decision of a node group", "group", g.name, "error", err)
		return
	}
	g.decision = nil
}

// groupObject reads the object of a node group.
func (r *runner) groupObject(g *demandGroup) (*structs.CustomObject, error) {
	return r.deps.State().CustomObjectByName(nil, structs.NodeScalingGroup, structs.NodeGroupKind, "", g.name)
}

// writeGroupStatus writes the status of the object with status.decision set
// to decision (removed when nil), keeping every other field, with the index
// the object was read at.
func (r *runner) writeGroupStatus(obj *structs.CustomObject, decision map[string]any) error {
	if r.deps.ObjectScaler == nil {
		return fmt.Errorf("no object writer")
	}
	status := structs.CopyObjectMap(obj.Status)
	if status == nil {
		status = map[string]any{}
	}
	if decision == nil {
		delete(status, decisionKey)
	} else {
		status[decisionKey] = decision
	}
	cas := obj.ModifyIndex
	return r.deps.ObjectScaler.UpdateObjectStatus(&structs.CustomObjectStatusRequest{
		Group:  obj.Group,
		Kind:   obj.Kind,
		Name:   obj.Name,
		Status: status,
		CAS:    &cas,
	})
}

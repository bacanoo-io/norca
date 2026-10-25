// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/hashicorp/nomad/nomad/autoscaler/sdk"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// Custom object targets (operator framework): a task group scaling policy
// can scale a custom object through its scale subresource instead of the
// group itself,
//
//	scaling {
//	  min = 1
//	  max = 7
//	  policy {
//	    target "custom-object" {
//	      kind      = "db.example.com/PostgresCluster"
//	      name      = "billing"
//	      namespace = "prod"   # defaults to the job's namespace
//	    }
//	    check "cpu" { ... }
//	  }
//	}
//
// The checks are evaluated on the task group of the policy (typically a
// group the object's controller derives from the object), and the decision
// is written to the object's scale subresource; the controller then derives
// the group count from the object. The cooldown and the oscillation
// protection are derived from the object's event log, where every change
// of the scale is recorded, so they survive leader changes as for groups.

const (
	// TargetCustomObject is the name of the target block of policies that
	// scale a custom object.
	TargetCustomObject = "custom-object"

	targetKeyKind      = "kind"
	targetKeyName      = "name"
	targetKeyNamespace = "namespace"

	// Reasons of the object log events written by the autoscaler besides
	// the "Scaled" events of the scale subresource.
	objectReasonSuspended = "ScalingSuspended"
	objectReasonDryRun    = "ScalingDryRun"
)

// ObjectScaler executes scaling actions on custom objects. On the server it
// is the internal CustomObject.Scale and CustomObject.RecordEvents RPCs,
// made with the autoscaler's system identity.
type ObjectScaler interface {
	ScaleObject(args *structs.CustomObjectScaleRequest) (*structs.CustomObjectScaleResponse, error)
	RecordObjectEvents(args *structs.CustomObjectEventsRequest) error
}

// objectTarget is a custom object scaled by a policy.
type objectTarget struct {
	group     string
	kind      string
	namespace string
	name      string
}

func (o *objectTarget) String() string {
	return fmt.Sprintf("%s/%s/%s/%s", o.namespace, o.group, o.kind, o.name)
}

// objectTargetFromPolicy returns the custom object target of a policy, nil
// if the policy scales its task group, or an error if the target block is
// invalid.
func objectTargetFromPolicy(p *structs.ScalingPolicy) (*objectTarget, error) {
	raw, ok := p.Policy[keyTarget]
	if !ok {
		return nil, nil
	}
	blocks := parseBlocks(normalizeBlocks(raw))
	body, ok := blocks[TargetCustomObject]
	if !ok {
		return nil, nil
	}
	if len(blocks) > 1 {
		return nil, errors.New("policy.target: only one target block is allowed")
	}
	cfg := parseBlock(normalizeBlocks(body))
	if cfg == nil {
		return nil, fmt.Errorf("policy.target[%s]: invalid block", TargetCustomObject)
	}
	str := func(key string) string {
		s, _ := cfg[key].(string)
		return strings.TrimSpace(s)
	}

	kindRef, name := str(targetKeyKind), str(targetKeyName)
	group, kind, err := structs.ParseObjectKindRef(kindRef)
	if err != nil {
		return nil, fmt.Errorf("policy.target[%s].kind: %v", TargetCustomObject, err)
	}
	if name == "" {
		return nil, fmt.Errorf("policy.target[%s]: missing name", TargetCustomObject)
	}
	namespace := str(targetKeyNamespace)
	if namespace == "" {
		namespace = p.Target[structs.ScalingTargetNamespace]
	}
	if namespace == "" {
		namespace = structs.DefaultNamespace
	}
	return &objectTarget{group: group, kind: kind, namespace: namespace, name: name}, nil
}

// readObjectTarget reads the state of a custom object target. A nil result
// means the object or its kind does not exist. The task group of the policy
// is read too, for the quota check.
func readObjectTarget(store *state.StateStore, key targetKey, o *objectTarget) (*targetState, error) {
	kind, err := store.ObjectKindByName(nil, o.group, o.kind)
	if err != nil {
		return nil, err
	}
	if kind == nil {
		return nil, nil
	}
	if kind.Scale == nil {
		return nil, fmt.Errorf("object kind %q has no scale subresource", kind.KindRef())
	}
	namespace := o.namespace
	if !kind.Namespaced {
		namespace = ""
	}
	obj, err := store.CustomObjectByName(nil, o.group, o.kind, namespace, o.name)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, nil
	}

	// The observed count, if the status reports it, like the running
	// allocations of a group; the desired count otherwise.
	scale := structs.ScaleOf(kind, obj)
	count := scale.Replicas
	if scale.StatusReplicas != nil {
		count = *scale.StatusReplicas
	}

	t := &targetState{
		status: &sdk.TargetStatus{Ready: !obj.Deleting(), Count: count, Meta: map[string]string{}},
	}

	if job, err := store.JobByID(nil, key.namespace, key.job); err == nil && job != nil {
		t.job = job
		t.group = job.LookupTaskGroup(key.group)
	}

	log, err := store.ObjectEventLog(nil, o.group, o.kind, namespace, o.name)
	if err != nil {
		return nil, err
	}
	if log != nil {
		t.events = objectScalingEvents(log.Events)
	}
	return t, nil
}

// objectScalingEvents converts the scaling entries of an object's event log
// into scaling events, newest first, so the cooldown and the oscillation
// protection work as for task groups.
func objectScalingEvents(events []*structs.ObjectEvent) []*structs.ScalingEvent {
	var out []*structs.ScalingEvent
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		meta := make(map[string]interface{}, len(e.Meta))
		for k, v := range e.Meta {
			meta[k] = v
		}
		se := &structs.ScalingEvent{
			Time:    e.LastTime,
			Message: e.Message,
			Meta:    meta,
		}
		switch {
		case e.Reason == structs.ObjectScaleReason:
			count, err1 := strconv.ParseInt(e.Meta[structs.ObjectEventMetaCount], 10, 64)
			prev, err2 := strconv.ParseInt(e.Meta[structs.ObjectEventMetaPreviousCount], 10, 64)
			if err1 != nil || err2 != nil {
				continue
			}
			se.Count = &count
			se.PreviousCount = prev
		case e.Meta[metaKeySuspendedUntil] != "":
			se.Error = true
		default:
			continue
		}
		out = append(out, se)
	}
	return out
}

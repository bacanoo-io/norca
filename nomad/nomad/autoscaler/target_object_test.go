// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

// fakeObjectScaler emulates the scale subresource and the event log of
// custom objects on the state store.
type fakeObjectScaler struct {
	t     *testing.T
	l     sync.Mutex
	store *state.StateStore
	clock *fakeClock
	index uint64
	reqs  []*structs.CustomObjectScaleRequest
}

func (f *fakeObjectScaler) object(group, kind, ns, name string) (*structs.ObjectKind, *structs.CustomObject) {
	k, err := f.store.ObjectKindByName(nil, group, kind)
	must.NoError(f.t, err)
	obj, err := f.store.CustomObjectByName(nil, group, kind, ns, name)
	must.NoError(f.t, err)
	return k, obj
}

func (f *fakeObjectScaler) ScaleObject(args *structs.CustomObjectScaleRequest) (*structs.CustomObjectScaleResponse, error) {
	f.l.Lock()
	defer f.l.Unlock()
	f.reqs = append(f.reqs, args)

	kind, obj := f.object(args.Group, args.Kind, args.Namespace, args.Name)
	prev := structs.ScaleOf(kind, obj).Replicas
	next := obj.Copy()
	must.NoError(f.t, structs.SetObjectPathValue(next, kind.Scale.SpecPath, float64(args.Replicas)))
	next.Generation++
	f.index++
	must.NoError(f.t, f.store.UpsertCustomObject(structs.MsgTypeTestSetup, f.index, next, obj.ModifyIndex))

	meta := map[string]string{
		structs.ObjectEventMetaCount:         strconv.FormatInt(args.Replicas, 10),
		structs.ObjectEventMetaPreviousCount: strconv.FormatInt(prev, 10),
	}
	for k, v := range args.Meta {
		meta[k] = v
	}
	f.index++
	must.NoError(f.t, f.store.AppendObjectEvents(structs.MsgTypeTestSetup, f.index, &structs.CustomObjectEventsStateRequest{
		Group: args.Group, Kind: args.Kind, Namespace: args.Namespace, Name: args.Name, ObjectID: obj.ID,
		Events: []*structs.ObjectEvent{{Reason: structs.ObjectScaleReason, Message: args.Message, Meta: meta}},
		Time:   f.clock.Now().UnixNano(),
	}))
	return &structs.CustomObjectScaleResponse{}, nil
}

func (f *fakeObjectScaler) RecordObjectEvents(args *structs.CustomObjectEventsRequest) error {
	f.l.Lock()
	defer f.l.Unlock()
	_, obj := f.object(args.Group, args.Kind, args.Namespace, args.Name)
	f.index++
	return f.store.AppendObjectEvents(structs.MsgTypeTestSetup, f.index, &structs.CustomObjectEventsStateRequest{
		Group: args.Group, Kind: args.Kind, Namespace: args.Namespace, Name: args.Name, ObjectID: obj.ID,
		Events: args.Events, Time: f.clock.Now().UnixNano(),
	})
}

func (f *fakeObjectScaler) requests() []*structs.CustomObjectScaleRequest {
	f.l.Lock()
	defer f.l.Unlock()
	return append([]*structs.CustomObjectScaleRequest(nil), f.reqs...)
}

func objectTargetDoc(name string) map[string]interface{} {
	return targetValuePolicy(map[string]interface{}{
		"target": []interface{}{
			map[string]interface{}{
				TargetCustomObject: []interface{}{
					map[string]interface{}{"kind": "db.example.com/PostgresCluster", "name": name},
				},
			},
		},
	})
}

func newObjectHarness(t *testing.T) (*harness, *fakeObjectScaler) {
	h := newHarness(t, structs.ScalingPolicyEngineBuiltin, objectTargetDoc("billing"), nil)
	objects := &fakeObjectScaler{t: t, store: h.store, clock: h.clock, index: 5000}
	h.deps.ObjectScaler = objects

	must.NoError(t, h.store.UpsertObjectKind(structs.MsgTypeTestSetup, 4000, &structs.ObjectKind{
		Group: "db.example.com", Kind: "PostgresCluster", Namespaced: true, Schema: `{"type":"object"}`,
		Scale: &structs.ObjectKindScale{SpecPath: "spec.replicas", StatusPath: "status.readyReplicas"},
	}))
	must.NoError(t, h.store.UpsertCustomObject(structs.MsgTypeTestSetup, 4001, &structs.CustomObject{
		ID: "pg-id", Group: "db.example.com", Kind: "PostgresCluster", Namespace: h.job.Namespace, Name: "billing",
		Generation: 1, Spec: map[string]any{"replicas": float64(2)},
	}, 0))
	h.newRunner()
	return h, objects
}

func TestAutoscaler_ObjectTarget(t *testing.T) {
	ci.Parallel(t)
	h, objects := newObjectHarness(t)

	// The policy's checks run on its group (fake APM at twice the target);
	// the decision goes to the object's scale subresource.
	must.NoError(t, h.evaluate())
	reqs := objects.requests()
	must.Len(t, 1, reqs)
	must.Eq(t, int64(4), reqs[0].Replicas)
	must.Eq(t, "billing", reqs[0].Name)
	must.Eq(t, h.job.Namespace, reqs[0].Namespace)
	must.Eq(t, structs.ScalingPolicyEngineBuiltin, reqs[0].Meta[metaKeyEngine])
	must.Len(t, 0, h.scaler.requests())

	obj, err := h.store.CustomObjectByName(nil, "db.example.com", "PostgresCluster", h.job.Namespace, "billing")
	must.NoError(t, err)
	must.Eq[any](t, float64(4), obj.Spec["replicas"])

	// The cooldown is derived from the object's event log, so a new leader
	// respects it.
	h.newRunner()
	h.clock.Advance(30 * time.Second)
	must.NoError(t, h.evaluate())
	must.Len(t, 1, objects.requests())

	// After the cooldown the observed count (the status) is the base.
	h.clock.Advance(time.Minute)
	status := obj.Copy()
	status.Status = map[string]any{"readyReplicas": float64(3)}
	must.NoError(t, h.store.UpsertCustomObject(structs.MsgTypeTestSetup, 4100, status, obj.ModifyIndex))
	must.NoError(t, h.evaluate())
	reqs = objects.requests()
	must.Len(t, 2, reqs)
	must.Eq(t, int64(6), reqs[1].Replicas)
}

func TestAutoscaler_ObjectTargetNotReady(t *testing.T) {
	ci.Parallel(t)
	h, objects := newObjectHarness(t)

	// An object being deleted is not scaled.
	obj, err := h.store.CustomObjectByName(nil, "db.example.com", "PostgresCluster", h.job.Namespace, "billing")
	must.NoError(t, err)
	deleting := obj.Copy()
	deleting.DeletionTime = 1
	must.NoError(t, h.store.UpsertCustomObject(structs.MsgTypeTestSetup, 4100, deleting, obj.ModifyIndex))
	must.NoError(t, h.evaluate())
	must.Len(t, 0, objects.requests())

	// A missing object is skipped.
	must.NoError(t, h.store.DeleteCustomObject(structs.MsgTypeTestSetup, 4101, "db.example.com", "PostgresCluster", h.job.Namespace, "billing", 0))
	must.NoError(t, h.evaluate())
	must.Len(t, 0, objects.requests())
}

func TestAutoscaler_ObjectTargetOscillation(t *testing.T) {
	ci.Parallel(t)
	h, objects := newObjectHarness(t)

	// Earlier scaling decisions in alternating directions, recorded in the
	// object's event log.
	_, obj := objects.object("db.example.com", "PostgresCluster", h.job.Namespace, "billing")
	for i, step := range [][2]int64{{2, 4}, {4, 2}, {2, 4}} {
		h.index++
		must.NoError(t, h.store.AppendObjectEvents(structs.MsgTypeTestSetup, h.index, &structs.CustomObjectEventsStateRequest{
			Group: "db.example.com", Kind: "PostgresCluster", Namespace: h.job.Namespace, Name: "billing", ObjectID: obj.ID,
			Events: []*structs.ObjectEvent{{Reason: structs.ObjectScaleReason, Message: strconv.Itoa(i), Meta: map[string]string{
				structs.ObjectEventMetaPreviousCount: strconv.FormatInt(step[0], 10),
				structs.ObjectEventMetaCount:         strconv.FormatInt(step[1], 10),
				metaKeyEngine:                        structs.ScalingPolicyEngineBuiltin,
			}}},
			Time: h.clock.Now().Add(-9 * time.Minute).Add(time.Duration(i) * time.Minute).UnixNano(),
		}))
	}

	// The next change of direction suspends the policy and records a
	// warning in the log instead of scaling.
	h.apm.value = 10
	must.NoError(t, h.evaluate())
	must.Len(t, 0, objects.requests())
	log, err := h.store.ObjectEventLog(nil, "db.example.com", "PostgresCluster", h.job.Namespace, "billing")
	must.NoError(t, err)
	last := log.Events[len(log.Events)-1]
	must.Eq(t, objectReasonSuspended, last.Reason)
	must.Eq(t, structs.ObjectEventTypeWarning, last.Type)

	// The suspension survives a leader change.
	h.newRunner()
	h.apm.value = 100
	must.NoError(t, h.evaluate())
	must.Len(t, 0, objects.requests())
}

func TestAutoscaler_ObjectTargetValidation(t *testing.T) {
	ci.Parallel(t)

	p := &structs.ScalingPolicy{
		Type:    structs.ScalingPolicyTypeHorizontal,
		Target:  map[string]string{structs.ScalingTargetNamespace: "prod", structs.ScalingTargetJob: "j", structs.ScalingTargetGroup: "g"},
		Policy:  objectTargetDoc("billing"),
		Enabled: true,
		Max:     5,
	}
	o, err := objectTargetFromPolicy(p)
	must.NoError(t, err)
	must.Eq(t, &objectTarget{group: "db.example.com", kind: "PostgresCluster", namespace: "prod", name: "billing"}, o)

	// Other targets are task group targets.
	p.Policy = targetValuePolicy(nil)
	o, err = objectTargetFromPolicy(p)
	must.NoError(t, err)
	must.Nil(t, o)

	p.Policy = objectTargetDoc("")
	must.ErrorContains(t, ValidatePolicy(p, nil), "missing name")
}

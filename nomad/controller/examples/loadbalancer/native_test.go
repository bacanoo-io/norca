// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/controller"
	"github.com/shoenig/test/must"
)

func (e *env) putNativeLB(name string, spec map[string]any) {
	e.b.PutObject(&api.CustomObject{Group: Kind.Group, Kind: Kind.Kind, Namespace: "default", Name: name, Spec: spec})
}

func (e *env) nativeObject(t *testing.T, name string) *controller.Object {
	co := e.b.GetObject(Kind, "default", name)
	if co == nil {
		return nil
	}
	obj, err := controller.ObjectFromCustomObject(Kind, co)
	must.NoError(t, err)
	return obj
}

func (e *env) nativeStatus(t *testing.T, name string) Status {
	obj := e.nativeObject(t, name)
	if obj == nil {
		return Status{}
	}
	var s Status
	must.NoError(t, obj.DecodeStatus(&s))
	return s
}

func TestLoadBalancer_NativeLifecycle(t *testing.T) {
	e := startOperatorWith(t, false, controller.StorageNative)
	e.cluster.setTargets("default", "web", Target{Address: "10.0.0.1", Port: 8001})
	e.putNativeLB("web", map[string]any{"service": "web", "port": 8080})

	waitFor(t, "job", func() bool { return e.b.GetJob("default", "lb-web") != nil })
	job := e.b.GetJob("default", "lb-web")
	must.Eq(t, "default/crd/norca-io/LoadBalancer/web", job.Meta[controller.OwnerMetaKey])

	e.cluster.setEndpoints("default", "lb-web", "192.168.1.10:8080")
	waitFor(t, "ready", func() bool { return e.nativeStatus(t, "web").IsTrue(controller.ConditionReady) })
	st := e.nativeStatus(t, "web")
	must.Eq(t, []string{"192.168.1.10:8080"}, st.Addresses)
	must.Eq(t, uint64(1), st.ObservedGeneration)
	must.Eq(t, "", st.ObservedSpecHash)

	// The status is written on the status endpoint; nothing is stored in
	// Variables except the proxy's targets.
	must.Positive(t, e.b.Calls("UpdateObjectStatus"))
	must.Eq(t, 0, e.b.Calls("ApplyObject"))
	must.Nil(t, e.b.GetVariable("default", Kind.Path("web")))

	// A spec change increases the generation and updates the job.
	e.putNativeLB("web", map[string]any{"service": "web", "port": 9090, "replicas": 2})
	waitFor(t, "job updated", func() bool {
		j := e.b.GetJob("default", "lb-web")
		return j.TaskGroups[0].Networks[0].ReservedPorts[0].Value == 9090 && *j.TaskGroups[0].Count == 2
	})
	waitFor(t, "generation observed", func() bool {
		obj := e.nativeObject(t, "web")
		return obj.Generation == 2 && e.nativeStatus(t, "web").Observed(obj)
	})

	// The object got the cleanup finalizer, the job the owner kind, and the
	// state changes are in the object's event log.
	must.Eq(t, []string{Finalizer}, e.nativeObject(t, "web").Finalizers)
	must.Eq(t, Kind.String(), e.b.GetJob("default", "lb-web").Meta[controller.OwnerKindMetaKey])
	waitFor(t, "ready event", func() bool {
		for _, ev := range e.b.ObjectEvents(Kind, "default", "web") {
			if ev.Reason == "Ready" {
				return true
			}
		}
		return false
	})

	// Deleting the object deletes the job first, then the finalizer is
	// removed and the object disappears.
	e.b.RemoveObject(Kind, "default", "web")
	waitFor(t, "object deleted", func() bool { return e.nativeObject(t, "web") == nil })
	must.Nil(t, e.b.GetJob("default", "lb-web"))
	must.Nil(t, e.b.GetVariable("default", targetsPath("lb-web")))
}

// TestLoadBalancer_NativeFinalizer checks that a load balancer deleted while
// the operator is down is removed when it comes back, and that a
// re-created object adopts the job left by an orphaning deletion.
func TestLoadBalancer_NativeFinalizer(t *testing.T) {
	e := startOperatorWith(t, false, controller.StorageNative)
	e.cluster.setTargets("default", "web", Target{Address: "10.0.0.1", Port: 8001})
	e.putNativeLB("web", map[string]any{"service": "web", "port": 8080})
	waitFor(t, "job", func() bool { return e.b.GetJob("default", "lb-web") != nil })
	waitFor(t, "finalizer", func() bool { return e.nativeObject(t, "web").HasFinalizer(Finalizer) })

	// Deleted while the operator is down: the object is only marked.
	e.stop()
	e.b.RemoveObject(Kind, "default", "web")
	time.Sleep(100 * time.Millisecond)
	obj := e.nativeObject(t, "web")
	must.NotNil(t, obj)
	must.True(t, obj.Deleting())
	must.NotNil(t, e.b.GetJob("default", "lb-web"))

	// The restarted operator finishes the cleanup.
	e2 := startOperatorOn(t, e.b, e.cluster, false, controller.StorageNative)
	waitFor(t, "object deleted", func() bool { return e2.nativeObject(t, "web") == nil })
	must.Nil(t, e2.b.GetJob("default", "lb-web"))

	// An object restored after its jobs survived (for example deleted in
	// the orphan mode, or restored from a backup) adopts the existing job
	// instead of creating a duplicate.
	e2.putNativeLB("api", map[string]any{"service": "web", "port": 8081})
	waitFor(t, "job", func() bool { return e2.b.GetJob("default", "lb-api") != nil })
	waitFor(t, "finalizer", func() bool { return e2.nativeObject(t, "api").HasFinalizer(Finalizer) })
	e2.stop()
	job := e2.b.GetJob("default", "lb-api")
	cur, err := e2.b.UpdateObjectFinalizers(context.Background(), Kind, "default", "api", nil, []string{Finalizer}, 0)
	must.NoError(t, err)
	must.NotNil(t, cur)
	e2.b.RemoveObject(Kind, "default", "api")
	must.Nil(t, e2.nativeObject(t, "api"))
	registrations := e2.b.Calls("RegisterJob")

	e3 := startOperatorOn(t, e2.b, e2.cluster, false, controller.StorageNative)
	e3.putNativeLB("api", map[string]any{"service": "web", "port": 8081})
	waitFor(t, "finalizer", func() bool {
		o := e3.nativeObject(t, "api")
		return o != nil && o.HasFinalizer(Finalizer)
	})
	time.Sleep(100 * time.Millisecond)
	must.Eq(t, registrations, e3.b.Calls("RegisterJob"))
	must.Eq(t, *job.ModifyIndex, *e3.b.GetJob("default", "lb-api").ModifyIndex)
	must.Eq(t, []string{"lb-api"}, e3.b.Jobs("default"))
}

func TestLoadBalancer_NativeDiscovery(t *testing.T) {
	e := startOperatorWith(t, true, controller.StorageNative)
	e.cluster.setTargets("default", "api.public", Target{Address: "10.0.0.9", Port: 443})

	// Native object names may contain dots.
	e.b.PutJob(markedJob("api", "api.public", "nomad", "8443"))
	waitFor(t, "object created from the mark", func() bool { return e.nativeObject(t, "api.public") != nil })
	obj := e.nativeObject(t, "api.public")
	must.Eq(t, managedByMarker, obj.Labels[labelManagedBy])
	waitFor(t, "proxy job", func() bool { return e.b.GetJob("default", "lb-api.public") != nil })

	// Once created, the object is not rewritten by later reconciles.
	applies := e.b.Calls("ApplyObject")
	time.Sleep(200 * time.Millisecond)
	must.Eq(t, applies, e.b.Calls("ApplyObject"))

	e.b.PutJob(markedJob("api", "api.public", "nomad", "9443"))
	waitFor(t, "object updated", func() bool {
		var s Spec
		o := e.nativeObject(t, "api.public")
		return o != nil && o.DecodeSpec(&s) == nil && s.Port == 9443 && o.Generation == 2
	})

	e.b.RemoveJob("default", "api", false)
	waitFor(t, "object deleted", func() bool { return e.nativeObject(t, "api.public") == nil })
	waitFor(t, "proxy deleted", func() bool { return e.b.GetJob("default", "lb-api.public") == nil })
}

// TestLoadBalancer_KindFiles checks that the example kind definition and
// its schema are well-formed.
func TestLoadBalancer_KindFiles(t *testing.T) {
	raw, err := os.ReadFile("loadbalancer.schema.json")
	must.NoError(t, err)
	var schema struct {
		Properties map[string]struct {
			Properties map[string]map[string]any `json:"properties"`
		} `json:"properties"`
	}
	must.NoError(t, json.Unmarshal(raw, &schema))
	spec := schema.Properties["spec"].Properties
	// Every field of Spec is declared, with the defaults of Spec.Defaults.
	for _, field := range []string{"service", "class", "port", "internal", "replicas", "datacenters"} {
		must.MapContainsKey(t, spec, field)
	}
	var defaults Spec
	defaults.Defaults()
	must.Eq[any](t, defaults.Class, spec["class"]["default"])
	must.Eq[any](t, float64(defaults.Replicas), spec["replicas"]["default"])

	kind, err := os.ReadFile("loadbalancer.kind.hcl")
	must.NoError(t, err)
	must.StrContains(t, string(kind), `group       = "`+Kind.Group+`"`)
	must.StrContains(t, string(kind), `kind "`+Kind.Kind+`"`)
}

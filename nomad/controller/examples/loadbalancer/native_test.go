// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
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

	// Deleting the object deletes the job.
	e.b.RemoveObject(Kind, "default", "web")
	waitFor(t, "job deleted", func() bool { return e.b.GetJob("default", "lb-web") == nil })
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

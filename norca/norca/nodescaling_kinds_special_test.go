// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

func checkSpec(check map[string]any) map[string]any {
	return map[string]any{"policy": map[string]any{"check": map[string]any{"c": check}}}
}

// TestNodeScalingKinds_SpecialCases checks the edges of the schema and of the
// rules: values on the border, fields that can not change, and what the API
// refuses for the built-in kinds.
func TestNodeScalingKinds_SpecialCases(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, nil)
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		snap, err := s.State().Snapshot()
		if err != nil {
			return false
		}
		g, _ := snap.ObjectKindByName(nil, structs.NodeScalingGroup, structs.NodeGroupKind)
		return g != nil
	}), wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))

	dry := func(name string, mutate func(map[string]any)) error {
		spec := validNodeGroupSpec()
		mutate(spec)
		_, err := c.apply(nodeGroupObject(name, spec), nil, true, "")
		return err
	}
	threshold := map[string]any{"threshold": map[string]any{}}

	valid := map[string]func(map[string]any){
		"a group that may not grow": func(sp map[string]any) { sp["max"] = 0 },
		"min equals max":            func(sp map[string]any) { sp["min"] = 4; sp["max"] = 4; sp["replicas"] = 4 },
		"replicas at the maximum":   func(sp map[string]any) { sp["replicas"] = 10 },
		"disabled":                  func(sp map[string]any) { sp["enabled"] = false },
		"a share of 100 percent":    func(sp map[string]any) { sp["headroom_percent"] = 100 },
		"a day count":               func(sp map[string]any) { sp["rolling"] = map[string]any{"max_node_age": "45d", "jitter": "15%"} },
		"a wide network":            func(sp map[string]any) { sp["network"] = "10.0.0.0/8" },
		"a node class":              func(sp map[string]any) { sp["node_class"] = "gpu" },
		"node attributes and meta": func(sp map[string]any) {
			sp["node"] = map[string]any{"meta": map[string]any{"tier": "x"}, "attributes": map[string]any{"unique.storage": "ssd"}}
		},
		"capacity with reserved": func(sp map[string]any) {
			sp["node"] = map[string]any{"capacity": map[string]any{"cpu": 4000, "memory": 8192}, "reserved": map[string]any{"cpu": 100}}
		},
		"scale down settings": func(sp map[string]any) {
			sp["policy"] = map[string]any{"scale_down": map[string]any{"selector": "empty", "utilization_below": 40}}
		},
		"a negative priority": func(sp map[string]any) { sp["priority"] = -1 },
		"a check that ignores errors": func(sp map[string]any) {
			sp["policy"] = checkSpec(map[string]any{"source": "norca", "query": "q", "on_error": "ignore", "strategy": threshold})["policy"]
		},
		"no policy block at all":        func(sp map[string]any) { delete(sp, "policy") },
		"an external group no provider": func(sp map[string]any) { sp["engine"] = "external"; delete(sp, "provider") },
	}
	for name, mutate := range valid {
		must.NoError(t, dry("v", mutate), must.Sprint(name))
	}

	invalid := map[string]func(map[string]any){
		"a weight of zero":          func(sp map[string]any) { sp["weight"] = 0 },
		"a share above 100 percent": func(sp map[string]any) { sp["headroom_percent"] = 101 },
		"a negative share":          func(sp map[string]any) { sp["headroom_percent"] = -1 },
		"negative headroom nodes":   func(sp map[string]any) { sp["headroom_nodes"] = -1 },
		"negative min":              func(sp map[string]any) { sp["min"] = -1 },
		"no maximum":                func(sp map[string]any) { delete(sp, "max") },
		"an empty pool":             func(sp map[string]any) { sp["pool"] = "" },
		"a fractional duration":     func(sp map[string]any) { sp["policy"] = map[string]any{"cooldown": "1.5h"} },
		"a bad jitter":              func(sp map[string]any) { sp["rolling"] = map[string]any{"jitter": "15"} },
		"zero unavailable":          func(sp map[string]any) { sp["rolling"] = map[string]any{"max_unavailable": 0} },
		"a bad rolling mode":        func(sp map[string]any) { sp["rolling"] = map[string]any{"mode": "fast"} },
		"a bad deletion policy":     func(sp map[string]any) { sp["deletion"] = map[string]any{"policy": "vanish"} },
		"a bad selector": func(sp map[string]any) {
			sp["policy"] = map[string]any{"scale_down": map[string]any{"selector": "random"}}
		},
		"a utilization of 120": func(sp map[string]any) {
			sp["policy"] = map[string]any{"scale_down": map[string]any{"utilization_below": 120}}
		},
		"a check with no strategy": func(sp map[string]any) { sp["policy"] = nodePolicySpec(map[string]any{}) },
		"a check with no query": func(sp map[string]any) {
			sp["policy"] = checkSpec(map[string]any{"source": "norca", "strategy": threshold})["policy"]
		},
		"a bad on_error": func(sp map[string]any) {
			sp["policy"] = checkSpec(map[string]any{"source": "norca", "query": "q", "on_error": "panic", "strategy": threshold})["policy"]
		},
		"a capacity with a string": func(sp map[string]any) {
			sp["node"] = map[string]any{"capacity": map[string]any{"cpu": "4000"}}
		},
		"an unknown node field":  func(sp map[string]any) { sp["node"] = map[string]any{"gpu": 1} },
		"a network with letters": func(sp map[string]any) { sp["network"] = "lab/24" },
		"an engine typo":         func(sp map[string]any) { sp["engine"] = "externl" },
	}
	for name, mutate := range invalid {
		must.Error(t, dry("i", mutate), must.Sprint(name))
	}

	// The class and the datacenter can not change.
	spec := validNodeGroupSpec()
	spec["node_class"] = "gpu"
	_, err := c.apply(nodeGroupObject("classed", spec), nil, false, "")
	must.NoError(t, err)
	_, err = c.apply(nodeGroupObject("classed", validNodeGroupSpec()), nil, false, "") // no class
	must.Error(t, err)
	spec = validNodeGroupSpec()
	spec["node_class"] = "gpu"
	spec["datacenter"] = "dc-b"
	_, err = c.apply(nodeGroupObject("classed", spec), nil, false, "")
	must.Error(t, err)

	// The rest of the spec can change.
	spec = validNodeGroupSpec()
	spec["node_class"] = "gpu"
	spec["max"] = 20
	spec["headroom_nodes"] = 2
	_, err = c.apply(nodeGroupObject("classed", spec), nil, false, "")
	must.NoError(t, err)

	// The definition of the kinds can neither be deleted nor replaced.
	req := &structs.ObjectKindDeleteRequest{
		Group: structs.NodeScalingGroup, Kind: structs.NodeGroupKind,
		WriteRequest: structs.WriteRequest{Region: "global"},
	}
	var resp structs.GenericResponse
	err = msgpackrpc.CallWithCodec(c.codec, "ObjectKind.DeleteObjectKind", req, &resp)
	must.Eq(t, 400, errCode(t, err))
	replaced := builtinNodeScalingKinds()[1].Copy()
	replaced.Schema = `{"type":"object"}`
	_, err = c.upsertKind(replaced, false, "")
	must.Eq(t, 400, errCode(t, err))

	// A provider with groups; a group with no name is refused.
	_, err = c.apply(&structs.CustomObject{
		Group: structs.NodeScalingGroup, Kind: structs.NodeProviderKind, Name: "p1",
		Spec: map[string]any{"endpoint": "http://127.0.0.1:9000", "groups": []any{
			map[string]any{"name": "fsn1", "max": 10, "parameters": map[string]any{"size": "cx32"}},
		}},
	}, nil, false, "")
	must.NoError(t, err)
	_, err = c.apply(&structs.CustomObject{
		Group: structs.NodeScalingGroup, Kind: structs.NodeProviderKind, Name: "p2",
		Spec: map[string]any{"endpoint": "https://x", "groups": []any{map[string]any{"max": 1}}},
	}, nil, true, "")
	must.Error(t, err)
}

// TestNodeScalingKinds_OperatorStatus checks that the status the node scaling
// operator writes passes the schema of the kind, and that a negative count is
// refused.
func TestNodeScalingKinds_OperatorStatus(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, nil)
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)
	c := &objectClient{t: t, codec: rpcClient(t, s)}
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		g, _ := s.State().ObjectKindByName(nil, structs.NodeScalingGroup, structs.NodeGroupKind)
		return g != nil
	}), wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))
	_, err := c.apply(nodeGroupObject("workers-dc-a", validNodeGroupSpec()), nil, false, "")
	must.NoError(t, err)

	write := func(status map[string]any) error {
		req := &structs.CustomObjectStatusRequest{
			Group: structs.NodeScalingGroup, Kind: structs.NodeGroupKind, Name: "workers-dc-a",
			Status: status, WriteRequest: structs.WriteRequest{Region: "global"},
		}
		var resp structs.CustomObjectWriteResponse
		return msgpackrpc.CallWithCodec(c.codec, "CustomObject.UpdateStatus", req, &resp)
	}
	must.NoError(t, write(map[string]any{
		"replicas": 1, "joining": 2, "failed": 0, "lastRequest": 3,
		"capacity":    map[string]any{"cpu": 4000, "memory": 8192, "disk": 40000},
		"allocatable": map[string]any{"cpu": 3900, "memory": 8000, "disk": 40000},
		"nodes": []any{
			map[string]any{"instance": "i-1", "state": "running", "request": "eu/workers-dc-a/3", "created": "2026-11-05T12:00:00Z", "node": "n1", "ready": true},
			map[string]any{"instance": "i-2", "state": "creating", "request": "eu/workers-dc-a/3", "created": "2026-11-05T12:00:00Z"},
		},
		"conditions": []any{
			map[string]any{"type": "ProviderAvailable", "status": "True", "reason": "Available", "lastTransitionTime": "2026-11-05T12:00:00Z"},
			map[string]any{"type": "Provisioned", "status": "False", "reason": "Quota", "message": "quota"},
		},
		"decision": map[string]any{"replicas": 3},
		"join":     map[string]any{"failures": 1, "revision": "a1b2c3d4e5f6", "resumed": 0, "lastFailure": "did not join"},
	}))
	must.Error(t, write(map[string]any{"join": map[string]any{"failures": -1}}))
	must.Error(t, write(map[string]any{"lastRequest": -1}))
}

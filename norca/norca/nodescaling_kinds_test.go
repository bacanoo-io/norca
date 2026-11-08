// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
	"time"
)

func nodeGroupObject(name string, spec map[string]any) *structs.CustomObject {
	return &structs.CustomObject{
		Group: structs.NodeScalingGroup,
		Kind:  structs.NodeGroupKind,
		Name:  name,
		Spec:  spec,
	}
}

func validNodeGroupSpec() map[string]any {
	return map[string]any{
		"pool": "workers", "datacenter": "dc-a", "max": 10, "provider": "lab",
	}
}

// TestNodeScalingKinds checks that the leader registers the kinds, that the
// objects are validated by the schema and the CEL rules, that the scale
// subresource works, and that the kinds can't be changed through the API.
func TestNodeScalingKinds(t *testing.T) {
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
		p, _ := snap.ObjectKindByName(nil, structs.NodeScalingGroup, structs.NodeProviderKind)
		return g != nil && p != nil
	}), wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))

	// A valid group gets the defaults.
	resp, err := c.apply(nodeGroupObject("workers-dc-a", validNodeGroupSpec()), nil, false, "")
	must.NoError(t, err)
	must.Eq[any](t, true, resp.Object.Spec["enabled"])
	must.Eq[any](t, false, resp.Object.Spec["enforce"])
	must.Eq[any](t, "builtin", resp.Object.Spec["engine"])
	must.Eq[any](t, float64(0), resp.Object.Spec["min"])

	invalid := map[string]func(map[string]any){
		"min above max":         func(sp map[string]any) { sp["min"] = 11 },
		"replicas above max":    func(sp map[string]any) { sp["replicas"] = 11 },
		"replicas below min":    func(sp map[string]any) { sp["min"] = 3; sp["replicas"] = 2 },
		"no provider":           func(sp map[string]any) { delete(sp, "provider") },
		"no pool":               func(sp map[string]any) { delete(sp, "pool") },
		"bad engine":            func(sp map[string]any) { sp["engine"] = "magic" },
		"bad network":           func(sp map[string]any) { sp["network"] = "10.0.0.0/33" },
		"bad duration":          func(sp map[string]any) { sp["join"] = map[string]any{"deadline": "soon"} },
		"unknown rolling field": func(sp map[string]any) { sp["rolling"] = map[string]any{"fast": true} },
		"bad deletion policy":   func(sp map[string]any) { sp["deletion"] = map[string]any{"policy": "explode"} },
		"bad key store":         func(sp map[string]any) { sp["join"] = map[string]any{"key_store": "hsm"} },
		"two strategies in check": func(sp map[string]any) {
			sp["policy"] = nodePolicySpec(map[string]any{"a": map[string]any{}, "b": map[string]any{}})
		},
	}
	for name, mutate := range invalid {
		spec := validNodeGroupSpec()
		mutate(spec)
		_, err := c.apply(nodeGroupObject("bad", spec), nil, true, "")
		must.Error(t, err, must.Sprint(name))
	}

	// An external autoscaler needs no provider.
	spec := validNodeGroupSpec()
	delete(spec, "provider")
	spec["engine"] = "external"
	_, err = c.apply(nodeGroupObject("external", spec), nil, true, "")
	must.NoError(t, err)

	// The nodes of a group may keep their keys in their TPMs.
	spec = validNodeGroupSpec()
	spec["join"] = map[string]any{"key_store": "tpm"}
	_, err = c.apply(nodeGroupObject("tpm", spec), nil, true, "")
	must.NoError(t, err)

	// A valid check.
	spec = validNodeGroupSpec()
	spec["policy"] = nodePolicySpec(map[string]any{"target-value": map[string]any{"target": 70}})
	_, err = c.apply(nodeGroupObject("checked", spec), nil, true, "")
	must.NoError(t, err)

	// The pool and the datacenter can't be changed.
	spec = validNodeGroupSpec()
	spec["pool"] = "other"
	_, err = c.apply(nodeGroupObject("workers-dc-a", spec), nil, false, "")
	must.Error(t, err)

	// The scale subresource is spec.replicas.
	scaled, err := scaleNodeGroup(c, "workers-dc-a", 4)
	must.NoError(t, err)
	must.Eq(t, int64(4), scaled.Scale.Replicas)
	_, err = scaleNodeGroup(c, "workers-dc-a", 11)
	must.Error(t, err)

	// A provider.
	_, err = c.apply(&structs.CustomObject{
		Group: structs.NodeScalingGroup, Kind: structs.NodeProviderKind, Name: "lab",
		Spec: map[string]any{"endpoint": "https://provider.example:8443"},
	}, nil, false, "")
	must.NoError(t, err)
	_, err = c.apply(&structs.CustomObject{
		Group: structs.NodeScalingGroup, Kind: structs.NodeProviderKind, Name: "bad",
		Spec: map[string]any{"endpoint": "ftp://x"},
	}, nil, true, "")
	must.Error(t, err)

	// The kinds are built in.
	kinds := builtinNodeScalingKinds()
	changed := kinds[0].Copy()
	changed.Description = "mine"
	_, err = c.upsertKind(changed, false, "")
	must.Eq(t, 400, errCode(t, err))
}

// nodePolicySpec is the policy block with one check on the allocated share
// whose strategy block is the given map.
func nodePolicySpec(strategy map[string]any) map[string]any {
	return map[string]any{"check": map[string]any{"allocated": map[string]any{
		"source": "norca", "query": "cluster.cpu_allocated", "strategy": strategy,
	}}}
}

func scaleNodeGroup(c *objectClient, name string, replicas int64) (*structs.CustomObjectScaleResponse, error) {
	req := &structs.CustomObjectScaleRequest{
		Group:        structs.NodeScalingGroup,
		Kind:         structs.NodeGroupKind,
		Name:         name,
		Replicas:     replicas,
		WriteRequest: structs.WriteRequest{Region: "global"},
	}
	var resp structs.CustomObjectScaleResponse
	err := msgpackrpc.CallWithCodec(c.codec, "CustomObject.Scale", req, &resp)
	return &resp, err
}

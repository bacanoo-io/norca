// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestResolveTopologyZone(t *testing.T) {
	ci.Parallel(t)

	node := &structs.Node{
		Datacenter: "dc1",
		Attributes: map[string]string{"platform.gce.zone": "europe-west3-a"},
		Meta:       map[string]string{"zone": "fsn1"},
	}

	must.Eq(t, "fsn1", resolveTopologyZone("${meta.zone}", node))
	must.Eq(t, "europe-west3-a", resolveTopologyZone("${attr.platform.gce.zone}", node))
	must.Eq(t, "dc1", resolveTopologyZone("${node.datacenter}", node))
	must.Eq(t, "rack-7", resolveTopologyZone("rack-7", node))
	must.Eq(t, "", resolveTopologyZone("${meta.missing}", node))
	must.Eq(t, "", resolveTopologyZone("${meta.zone}", nil))
}

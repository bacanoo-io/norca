// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

// The node list stub carries the Norca version attribute of the client, so
// lists can show it without fetching every node.
func TestNode_Stub_NorcaVersion(t *testing.T) {
	ci.Parallel(t)

	node := &Node{
		ID:         "n1",
		Attributes: map[string]string{"nomad.version": "1.6.5", "norca.version": "0.1.0", "os.name": "linux"},
	}

	stub := node.Stub(nil)
	must.Eq(t, "1.6.5", stub.Version)
	must.Eq(t, map[string]string{"norca.version": "0.1.0"}, stub.Attributes)

	stub = node.Stub(&NodeStubFields{OS: true})
	must.Eq(t, map[string]string{"norca.version": "0.1.0", "os.name": "linux"}, stub.Attributes)

	// A client that predates Norca has no attribute and no attributes map.
	old := &Node{ID: "n2", Attributes: map[string]string{"nomad.version": "1.6.5"}}
	must.Nil(t, old.Stub(nil).Attributes)
}

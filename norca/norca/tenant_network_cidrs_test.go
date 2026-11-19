// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"testing"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
)

// The namespace peers of tenant-isolation match allocations on the same node
// only (traffic from other nodes arrives with the address of the node), so
// the tenant could not let in its own instances on other nodes or a load
// balancer, and host networked groups could not be placed (review finding
// TEN-11). allow_from_cidrs adds a CIDR peer to the rule, and required =
// false makes it best_effort; a change of either is written to the policy.
func TestTenant_NetworkCIDRsAndRequired(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	store := c.s.State()

	policy := func() *structs.NetworkPolicy {
		t.Helper()
		p, err := store.NetworkPolicyByName(nil, "team-n", structs.TenantNetworkPolicyName)
		must.NoError(t, err)
		must.NotNil(t, p)
		return p
	}
	cidrPeer := func(p *structs.NetworkPolicy) []string {
		for _, peer := range p.Ingress[0].Peers {
			if len(peer.CIDR) > 0 {
				return peer.CIDR
			}
		}
		return nil
	}

	_, err := c.apply("", structs.TenantKind, "team-n", map[string]any{
		"network": map[string]any{
			"isolation":        "namespace",
			"allow_from_cidrs": []any{"10.0.1.0/24", "10.0.0.0/24", "10.0.1.0/24"},
		},
	})
	must.NoError(t, err)
	waitTenant(c, "team-n", "ready", tenantReady)
	p := policy()
	must.Eq(t, []string{"10.0.0.0/24", "10.0.1.0/24"}, cidrPeer(p))
	must.Eq(t, structs.NetworkPolicyEnforcementRequired, p.Enforcement)

	// required = false: best_effort; the CIDRs removed: no CIDR peer.
	_, err = c.apply("", structs.TenantKind, "team-n", map[string]any{
		"network": map[string]any{"isolation": "namespace", "required": false},
	})
	must.NoError(t, err)
	testutil.WaitForResult(func() (bool, error) {
		p := policy()
		return p.Enforcement == structs.NetworkPolicyEnforcementBestEffort && cidrPeer(p) == nil,
			fmt.Errorf("enforcement %q, CIDRs %v", p.Enforcement, cidrPeer(p))
	}, func(err error) { t.Fatal(err) })

	// Without the fields the tenant is as before: required, no CIDR peer.
	_, err = c.apply("", structs.TenantKind, "team-n", map[string]any{
		"network": map[string]any{"isolation": "namespace"},
	})
	must.NoError(t, err)
	testutil.WaitForResult(func() (bool, error) {
		p := policy()
		return p.Enforcement == structs.NetworkPolicyEnforcementRequired, fmt.Errorf("enforcement %q", p.Enforcement)
	}, func(err error) { t.Fatal(err) })
}

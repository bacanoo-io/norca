// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// newTenantRaceTest returns a tenant test server whose tenant operator runs
// write right before it writes the resource target, once: what somebody else
// commits between the read of the pass and its write.
func newTenantRaceTest(t *testing.T, target tenantResourceRef, write func(t *testing.T, s *Server)) *certTest {
	t.Helper()
	var srv atomic.Pointer[Server]
	var once sync.Once
	s, root, cleanup := TestACLServer(t, func(c *Config) {
		c.tenantBeforeWrite = func(ref tenantResourceRef) {
			if ref.key() == target.key() {
				once.Do(func() { write(t, srv.Load()) })
			}
		}
	})
	t.Cleanup(cleanup)
	srv.Store(s)
	testutil.WaitForLeader(t, s.RPC)
	waitTenantKind(t, s)
	return &certTest{t: t, s: s, token: root.SecretID}
}

// raftWrite commits a write as an operator would, past the endpoints.
func raftWrite(t *testing.T, s *Server, msgType structs.MessageType, req any) {
	t.Helper()
	_, _, err := s.raftApply(msgType, req)
	must.NoError(t, err)
}

// A resource somebody else creates between the read of a tenant's pass and
// the tenant's create stays theirs, as it is: the tenant creates only what
// does not exist, and the next pass finds the resource and reports the
// conflict (review finding ATM-1 = CON-2).
func TestTenantOperator_ForeignCreateBeforeWrite(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		name    string
		tenant  string
		spec    map[string]any
		target  tenantResourceRef
		foreign func(t *testing.T, s *Server)
		kept    func(t *testing.T, store *state.StateStore)
	}{
		{
			name:   "namespace",
			tenant: "team-n",
			spec:   map[string]any{"namespace": map[string]any{"description": "Tenant N"}},
			target: tenantResourceRef{Type: structs.TenantResourceNamespace, Name: "team-n"},
			foreign: func(t *testing.T, s *Server) {
				raftWrite(t, s, structs.NamespaceUpsertRequestType, &structs.NamespaceUpsertRequest{
					Namespaces: []*structs.Namespace{{Name: "team-n", Description: "made by hand", Meta: map[string]string{"owner": "ops"}}},
				})
			},
			kept: func(t *testing.T, store *state.StateStore) {
				ns, err := store.NamespaceByName(nil, "team-n")
				must.NoError(t, err)
				must.NotNil(t, ns)
				must.Eq(t, "made by hand", ns.Description)
				must.Eq(t, map[string]string{"owner": "ops"}, ns.Meta)
			},
		},
		{
			name:   "quota",
			tenant: "team-q",
			spec: map[string]any{"quota": map[string]any{"region": map[string]any{
				"global": map[string]any{"cpu": 8000},
			}}},
			target: tenantResourceRef{Type: structs.TenantResourceQuota, Name: "team-q"},
			foreign: func(t *testing.T, s *Server) {
				raftWrite(t, s, structs.QuotaSpecUpsertRequestType|structs.IgnoreUnknownTypeFlag, &structs.QuotaSpecUpsertRequest{
					Quotas: []*structs.QuotaSpec{{Name: "team-q", Description: "made by hand", Limits: []*structs.QuotaLimit{
						{Region: "global", RegionLimit: &structs.Resources{CPU: 100}},
					}}},
				})
			},
			kept: func(t *testing.T, store *state.StateStore) {
				q, err := store.QuotaSpecByName(nil, "team-q")
				must.NoError(t, err)
				must.NotNil(t, q)
				must.Eq(t, "made by hand", q.Description)
				must.Eq(t, 100, q.Limits[0].RegionLimit.CPU)
			},
		},
		{
			name:   "ACL policy",
			tenant: "team-p",
			spec:   map[string]any{"roles": map[string]any{"developer": []any{"submit-job"}}},
			target: tenantResourceRef{Type: structs.TenantResourceACLPolicy, Name: "team-p-developer"},
			foreign: func(t *testing.T, s *Server) {
				raftWrite(t, s, structs.ACLPolicyUpsertRequestType, &structs.ACLPolicyUpsertRequest{
					Policies: []*structs.ACLPolicy{{Name: "team-p-developer", Description: "made by hand", Rules: `node { policy = "read" }`}},
				})
			},
			kept: func(t *testing.T, store *state.StateStore) {
				pol, err := store.ACLPolicyByName(nil, "team-p-developer")
				must.NoError(t, err)
				must.NotNil(t, pol)
				must.Eq(t, "made by hand", pol.Description)
				must.Eq(t, `node { policy = "read" }`, pol.Rules)
			},
		},
		{
			name:   "network policy",
			tenant: "team-w",
			spec:   map[string]any{"network": map[string]any{"isolation": "namespace"}},
			target: tenantResourceRef{Type: structs.TenantResourceNetworkPolicy, Namespace: "team-w", Name: structs.TenantNetworkPolicyName},
			foreign: func(t *testing.T, s *Server) {
				raftWrite(t, s, structs.NetworkPolicyUpsertRequestType|structs.IgnoreUnknownTypeFlag, &structs.NetworkPolicyUpsertRequest{
					NetworkPolicies: []*structs.NetworkPolicy{{
						Name: structs.TenantNetworkPolicyName, Namespace: "team-w", Description: "made by hand",
						DefaultDeny: []string{structs.NetworkPolicyDirectionEgress},
					}},
				})
			},
			kept: func(t *testing.T, store *state.StateStore) {
				np, err := store.NetworkPolicyByName(nil, "team-w", structs.TenantNetworkPolicyName)
				must.NoError(t, err)
				must.NotNil(t, np)
				must.Eq(t, "made by hand", np.Description)
				must.Eq(t, []string{structs.NetworkPolicyDirectionEgress}, np.DefaultDeny)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ci.Parallel(t)
			c := newTenantRaceTest(t, tc.target, tc.foreign)

			_, err := c.apply("", structs.TenantKind, tc.tenant, tc.spec)
			must.NoError(t, err)
			obj := waitTenant(c, tc.tenant, "ready or in conflict", func(o *structs.CustomObject) bool {
				return tenantReady(o) || objectConditionTrue(o.Status, structs.TenantConditionConflict)
			})

			tc.kept(t, c.s.State())
			must.True(t, objectConditionTrue(obj.Status, structs.TenantConditionConflict),
				must.Sprintf("the tenant does not report the conflict: %v", obj.Status))
			must.SliceNotContains(t, tenantResources(obj), tc.target.key())
		})
	}
}

// A change somebody else makes to a namespace a tenant took over, between
// the read of the tenant's pass and its update, is not lost: the update is
// refused, and the next pass writes the tenant's fields into the namespace
// as it is then (review finding ATM-1 = CON-2).
func TestTenantOperator_AdoptedChangedBeforeUpdate(t *testing.T) {
	ci.Parallel(t)
	target := tenantResourceRef{Type: structs.TenantResourceNamespace, Name: "shared"}
	c := newTenantRaceTest(t, target, func(t *testing.T, s *Server) {
		ns, err := s.State().NamespaceByName(nil, "shared")
		must.NoError(t, err)
		changed := ns.Copy()
		changed.Capabilities = &structs.NamespaceCapabilities{EnabledTaskDrivers: []string{"docker"}}
		raftWrite(t, s, structs.NamespaceUpsertRequestType, &structs.NamespaceUpsertRequest{
			Namespaces: []*structs.Namespace{changed},
		})
	})

	var gen structs.GenericResponse
	must.NoError(t, c.s.RPC("Namespace.UpsertNamespaces", &structs.NamespaceUpsertRequest{
		Namespaces: []*structs.Namespace{{Name: "shared", Description: "made by hand"}}, WriteRequest: c.write(""),
	}, &gen))

	_, err := c.apply("", structs.TenantKind, "team-u", map[string]any{
		"namespace": map[string]any{"name": "shared", "description": "Tenant U"},
		"adopt":     []any{"namespace"},
	})
	must.NoError(t, err)
	obj := waitTenant(c, "team-u", "ready", tenantReady)
	must.SliceContains(t, tenantResources(obj), target.key())

	ns, err := c.s.State().NamespaceByName(nil, "shared")
	must.NoError(t, err)
	must.Eq(t, "Tenant U", ns.Description)
	must.NotNil(t, ns.Capabilities, must.Sprint("the change made between the read and the update is lost"))
	must.Eq(t, []string{"docker"}, ns.Capabilities.EnabledTaskDrivers)
}

// renameRole renames the ACL role of the name, as somebody else would: by
// its ID, with everything else as it is.
func renameRole(t *testing.T, s *Server, name, to string) {
	t.Helper()
	role, err := s.State().GetACLRoleByName(nil, name)
	must.NoError(t, err)
	must.NotNil(t, role)
	renamed := role.Copy()
	renamed.Name = to
	renamed.SetHash()
	raftWrite(t, s, structs.ACLRolesUpsertRequestType, &structs.ACLRolesUpsertRequest{ACLRoles: []*structs.ACLRole{renamed}})
}

// A change somebody else makes to an ACL role a tenant took over, between the
// read of the tenant's pass and its update, is not overwritten: the tenant
// changes the role by its ID only while it is as the pass read it. The role
// was written by its ID without a check, so a role renamed in between got the
// tenant's name, description and policies back (review finding ATM-26). The
// next pass finds no role of the name and creates the tenant's own.
func TestTenantOperator_RoleChangedBeforeUpdate(t *testing.T) {
	ci.Parallel(t)
	target := tenantResourceRef{Type: structs.TenantResourceACLRole, Name: "team-r-dev"}
	c := newTenantRaceTest(t, target, func(t *testing.T, s *Server) {
		renameRole(t, s, "team-r-dev", "ops-dev")
	})
	tenantByHand{c}.policyAndRole("team-r-dev")
	byHand, err := c.s.State().GetACLRoleByName(nil, "team-r-dev")
	must.NoError(t, err)
	must.NotNil(t, byHand)

	_, err = c.apply("", structs.TenantKind, "team-r", map[string]any{
		"namespace": map[string]any{"description": "Tenant R"},
		"roles":     map[string]any{"dev": []any{"submit-job"}},
		"adopt":     []any{"acl-policy", "acl-role"},
	})
	must.NoError(t, err)
	// The pass whose update is refused is not ready; the one after it is.
	waitTenant(c, "team-r", "ready", tenantReady)

	renamed, err := c.s.State().GetACLRoleByID(nil, byHand.ID)
	must.NoError(t, err)
	must.NotNil(t, renamed)
	must.Eq(t, "ops-dev", renamed.Name, must.Sprint("the rename made between the read and the update is lost"))
	must.Eq(t, "made by hand", renamed.Description, must.Sprint("the tenant wrote a role that changed since it read it"))
	must.Len(t, 1, renamed.Policies)
	must.Eq(t, "team-r-dev-other", renamed.Policies[0].Name)

	waitTenant(c, "team-r", "ready with a role of its own", func(o *structs.CustomObject) bool {
		id := tenantRefs(o)[target.key()].ID
		return tenantReady(o) && id != "" && id != byHand.ID
	})
	own, err := c.s.State().GetACLRoleByName(nil, "team-r-dev")
	must.NoError(t, err)
	must.NotNil(t, own)
	must.NotEq(t, byHand.ID, own.ID)
}

// The give-back of an ACL role a tenant took over is a check-and-set too: a
// role renamed between the read of the give-back and its write keeps its new
// name, and the next pass gives it back under that name. The give-back wrote
// the role by its ID with the name it had read (review finding ATM-26).
func TestTenantOperator_RoleChangedBeforeGiveBack(t *testing.T) {
	ci.Parallel(t)
	c, armed := newTenantArmedTest(t, "acl-role//team-v-dev")
	tenantByHand{c}.policyAndRole("team-v-dev")
	byHand, err := c.s.State().GetACLRoleByName(nil, "team-v-dev")
	must.NoError(t, err)
	must.NotNil(t, byHand)

	_, err = c.apply("", structs.TenantKind, "team-v", map[string]any{
		"namespace": map[string]any{"description": "V1"},
		"roles":     map[string]any{"dev": []any{"submit-job"}},
		"adopt":     []any{"acl-policy", "acl-role"},
	})
	must.NoError(t, err)
	obj := waitGeneration(c, "team-v")
	must.SliceContains(t, tenantAdopted(obj), "acl-role//team-v-dev")
	written, err := c.s.State().GetACLRoleByID(nil, byHand.ID)
	must.NoError(t, err)
	must.NotEq(t, "made by hand", written.Description)

	// The role leaves the spec; while the tenant gives it back, somebody
	// renames it.
	rename := func(s *Server) { renameRole(t, s, "team-v-dev", "ops-dev") }
	armed.Store(&rename)
	_, err = c.apply("", structs.TenantKind, "team-v", map[string]any{"namespace": map[string]any{"description": "V2"}})
	must.NoError(t, err)
	obj = waitTenant(c, "team-v", "ready without the role", func(o *structs.CustomObject) bool {
		_, listed := tenantRefs(o)["acl-role//team-v-dev"]
		return tenantReady(o) && !listed
	})
	must.Nil(t, armed.Load(), must.Sprint("the role was not renamed during the give-back"))

	role, err := c.s.State().GetACLRoleByID(nil, byHand.ID)
	must.NoError(t, err)
	must.NotNil(t, role)
	must.Eq(t, "ops-dev", role.Name, must.Sprint("the rename made between the read and the write of the give-back is lost"))
	must.Eq(t, "made by hand", role.Description, must.Sprint("the role was not given back as the tenant found it"))
	must.Len(t, 1, role.Policies)
	must.Eq(t, "team-v-dev-other", role.Policies[0].Name)
}

// What a tenant gives back is what the resource held when the tenant first
// wrote it, not what an earlier pass read: a change somebody else makes to a
// resource a tenant took over, between the read of the adopting pass and the
// tenant's first write, is refused by the check-and-set, and the status must
// not keep the original of that first read. It did: the next pass wrote the
// tenant's fields over the change, and the give-back restored what the first
// pass had read, so the change was lost for good (review finding ATM-26b).
func TestTenantOperator_AdoptedChangedBeforeFirstWrite(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		name   string
		tenant string
		spec   map[string]any
		target tenantResourceRef
		byHand func(h tenantByHand)
		change func(t *testing.T, s *Server)
		live   func(t *testing.T, store *state.StateStore) *tenantOriginal
	}{
		{
			name:   "namespace",
			tenant: "team-b1",
			spec: map[string]any{
				"namespace": map[string]any{"name": "shared-b1", "description": "Tenant B1"},
				"adopt":     []any{"namespace"},
			},
			target: tenantResourceRef{Type: structs.TenantResourceNamespace, Name: "shared-b1"},
			byHand: func(h tenantByHand) { h.namespace("shared-b1", "", map[string]string{"team": "old"}) },
			change: func(t *testing.T, s *Server) {
				ns, err := s.State().NamespaceByName(nil, "shared-b1")
				must.NoError(t, err)
				changed := ns.Copy()
				changed.Description, changed.Meta = "changed in between", map[string]string{"team": "new"}
				changed.SetHash()
				raftWrite(t, s, structs.NamespaceUpsertRequestType, &structs.NamespaceUpsertRequest{Namespaces: []*structs.Namespace{changed}})
			},
			live: func(t *testing.T, store *state.StateStore) *tenantOriginal {
				ns, err := store.NamespaceByName(nil, "shared-b1")
				must.NoError(t, err)
				return namespaceOriginal(ns)
			},
		},
		{
			name:   "quota",
			tenant: "team-b2",
			spec: map[string]any{
				"quota": map[string]any{"region": map[string]any{"global": map[string]any{"cpu": 8000}}},
				"adopt": []any{"quota"},
			},
			target: tenantResourceRef{Type: structs.TenantResourceQuota, Name: "team-b2"},
			byHand: func(h tenantByHand) { h.quota("team-b2") },
			change: func(t *testing.T, s *Server) {
				q, err := s.State().QuotaSpecByName(nil, "team-b2")
				must.NoError(t, err)
				changed := q.Copy()
				changed.Description = "changed in between"
				changed.Limits[0].RegionLimit.CPU = 300
				changed.SetHash()
				raftWrite(t, s, structs.QuotaSpecUpsertRequestType|structs.IgnoreUnknownTypeFlag,
					&structs.QuotaSpecUpsertRequest{Quotas: []*structs.QuotaSpec{changed}})
			},
			live: func(t *testing.T, store *state.StateStore) *tenantOriginal {
				q, err := store.QuotaSpecByName(nil, "team-b2")
				must.NoError(t, err)
				return quotaOriginal(q)
			},
		},
		{
			name:   "ACL policy",
			tenant: "team-b3",
			spec: map[string]any{
				"roles": map[string]any{"dev": []any{"submit-job"}},
				"adopt": []any{"acl-policy"},
			},
			target: tenantResourceRef{Type: structs.TenantResourceACLPolicy, Name: "team-b3-dev"},
			byHand: func(h tenantByHand) { h.policy("team-b3-dev", `node { policy = "read" }`) },
			change: func(t *testing.T, s *Server) {
				pol, err := s.State().ACLPolicyByName(nil, "team-b3-dev")
				must.NoError(t, err)
				changed := *pol
				changed.Description, changed.Rules = "changed in between", `agent { policy = "read" }`
				changed.SetHash()
				raftWrite(t, s, structs.ACLPolicyUpsertRequestType, &structs.ACLPolicyUpsertRequest{Policies: []*structs.ACLPolicy{&changed}})
			},
			live: func(t *testing.T, store *state.StateStore) *tenantOriginal {
				pol, err := store.ACLPolicyByName(nil, "team-b3-dev")
				must.NoError(t, err)
				return policyOriginal(pol)
			},
		},
		{
			name:   "ACL role",
			tenant: "team-b4",
			spec: map[string]any{
				"roles": map[string]any{"dev": []any{"submit-job"}},
				"adopt": []any{"acl-policy", "acl-role"},
			},
			target: tenantResourceRef{Type: structs.TenantResourceACLRole, Name: "team-b4-dev"},
			byHand: func(h tenantByHand) {
				h.policyAndRole("team-b4-dev")
				h.policy("team-b4-ops", `node { policy = "read" }`)
			},
			change: func(t *testing.T, s *Server) {
				role, err := s.State().GetACLRoleByName(nil, "team-b4-dev")
				must.NoError(t, err)
				changed := role.Copy()
				changed.Description = "changed in between"
				changed.Policies = []*structs.ACLRolePolicyLink{{Name: "team-b4-ops"}}
				changed.SetHash()
				raftWrite(t, s, structs.ACLRolesUpsertRequestType, &structs.ACLRolesUpsertRequest{ACLRoles: []*structs.ACLRole{changed}})
			},
			live: func(t *testing.T, store *state.StateStore) *tenantOriginal {
				role, err := store.GetACLRoleByName(nil, "team-b4-dev")
				must.NoError(t, err)
				return roleOriginal(role)
			},
		},
		{
			name:   "network policy",
			tenant: "team-b5",
			spec: map[string]any{
				"namespace": map[string]any{"description": "Tenant B5"},
				"network":   map[string]any{"isolation": "namespace"},
				"adopt":     []any{"namespace", "network-policy"},
			},
			target: tenantResourceRef{Type: structs.TenantResourceNetworkPolicy, Namespace: "team-b5", Name: structs.TenantNetworkPolicyName},
			byHand: func(h tenantByHand) {
				h.namespace("team-b5", "", nil)
				h.networkPolicy("team-b5")
			},
			change: func(t *testing.T, s *Server) {
				np, err := s.State().NetworkPolicyByName(nil, "team-b5", structs.TenantNetworkPolicyName)
				must.NoError(t, err)
				changed := np.Copy()
				changed.Description = "changed in between"
				changed.Ingress[0].Peers[0].CIDR = []string{"198.51.100.0/24"}
				raftWrite(t, s, structs.NetworkPolicyUpsertRequestType|structs.IgnoreUnknownTypeFlag,
					&structs.NetworkPolicyUpsertRequest{NetworkPolicies: []*structs.NetworkPolicy{changed}})
			},
			live: func(t *testing.T, store *state.StateStore) *tenantOriginal {
				np, err := store.NetworkPolicyByName(nil, "team-b5", structs.TenantNetworkPolicyName)
				must.NoError(t, err)
				return networkPolicyOriginal(np)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ci.Parallel(t)
			// What the resource holds after the change made in between.
			var between atomic.Pointer[tenantOriginal]
			c := newTenantRaceTest(t, tc.target, func(t *testing.T, s *Server) {
				tc.change(t, s)
				between.Store(tc.live(t, s.State()))
			})
			store := c.s.State()
			tc.byHand(tenantByHand{c})
			found := tc.live(t, store)
			must.NotNil(t, found)

			_, err := c.apply("", structs.TenantKind, tc.tenant, tc.spec)
			must.NoError(t, err)
			// The pass whose write is refused is not ready; the one after
			// it is.
			obj := waitTenant(c, tc.tenant, "ready", tenantReady)
			changed := between.Load()
			must.NotNil(t, changed, must.Sprint("the resource was not changed before the first write"))
			must.NotEq(t, found.fingerprint(), changed.fingerprint())
			must.NotEq(t, changed.fingerprint(), tc.live(t, store).fingerprint(), must.Sprint("the tenant did not write the resource"))

			ref, ok := tenantRefs(obj)[tc.target.key()]
			must.True(t, ok, must.Sprintf("the tenant does not list %s: %v", tc.target, obj.Status))
			must.True(t, ref.Adopted)
			must.NotNil(t, ref.Original)
			must.Eq(t, changed.fingerprint(), ref.Original.fingerprint(), must.Sprintf(
				"the status keeps what the first pass read, not what the tenant wrote over: %+v", ref.Original))
			must.Eq(t, "", ref.Writing, must.Sprint("the tenant wrote the resource"))

			must.NoError(t, c.delete("", structs.TenantKind, tc.tenant))
			waitTenantGone(c, tc.tenant)
			back := tc.live(t, store)
			must.NotNil(t, back, must.Sprint("the resource the tenant took over is gone"))
			must.Eq(t, changed.fingerprint(), back.fingerprint(), must.Sprintf(
				"the change made between the read and the first write is lost: given back as %+v", back))
		})
	}
}

// The original of a resource a tenant took over is frozen by the tenant's
// first write also when the status write that would have said so was lost:
// the tenant knows its own write by what it wrote down, before the write, that
// it was writing, and does not take what it wrote itself for the original
// (review finding ATM-26b). With a quota of its own the tenant writes the
// quota the namespace names; without one the namespace keeps what it names.
func TestTenantOperator_AdoptedWrittenStatusLost(t *testing.T) {
	ci.Parallel(t)

	for _, withQuota := range []bool{true, false} {
		name := map[bool]string{true: "with a quota", false: "without a quota"}[withQuota]
		t.Run(name, func(t *testing.T) {
			ci.Parallel(t)
			// The status as the pass wrote it before its first write: what
			// the tenant took and what it is writing.
			var srv atomic.Pointer[Server]
			var intent atomic.Pointer[structs.CustomObject]
			var once sync.Once
			s, root, cleanup := TestACLServer(t, func(c *Config) {
				c.tenantBeforeWrite = func(tenantResourceRef) {
					once.Do(func() {
						obj, err := srv.Load().State().CustomObjectByName(nil, structs.OperatorsGroup, structs.TenantKind, "", "team-ls")
						if err == nil && obj != nil {
							intent.Store(obj.Copy())
						}
					})
				}
			})
			t.Cleanup(cleanup)
			srv.Store(s)
			testutil.WaitForLeader(t, s.RPC)
			waitTenantKind(t, s)
			c := &certTest{t: t, s: s, token: root.SecretID}
			store := s.State()
			hand := tenantByHand{c}

			hand.quota("old-quota")
			hand.namespace("kept-ls", "old-quota", map[string]string{"team": "old"})
			hand.policyAndRole("team-ls-dev")
			hand.networkPolicy("kept-ls")
			spec := map[string]any{
				"namespace": map[string]any{"name": "kept-ls", "description": "Team LS", "meta": map[string]any{"owner": "ls"}},
				"roles":     map[string]any{"dev": []any{"submit-job"}},
				"network":   map[string]any{"isolation": "namespace"},
				"adopt":     []any{"namespace", "quota", "acl-policy", "acl-role", "network-policy"},
			}
			keys := []string{"acl-policy//team-ls-dev", "acl-role//team-ls-dev", "namespace//kept-ls", "network-policy/kept-ls/tenant-isolation"}
			if withQuota {
				hand.quota("team-ls")
				spec["quota"] = map[string]any{"region": map[string]any{"global": map[string]any{"cpu": 8000, "allocations": 5}}}
				keys = append(keys, "quota//team-ls")
			}
			live := func() map[string]string {
				ns, _ := store.NamespaceByName(nil, "kept-ls")
				pol, _ := store.ACLPolicyByName(nil, "team-ls-dev")
				role, _ := store.GetACLRoleByName(nil, "team-ls-dev")
				np, _ := store.NetworkPolicyByName(nil, "kept-ls", structs.TenantNetworkPolicyName)
				out := map[string]string{
					"namespace//kept-ls":                      namespaceOriginal(ns).fingerprint(),
					"acl-policy//team-ls-dev":                 policyOriginal(pol).fingerprint(),
					"acl-role//team-ls-dev":                   roleOriginal(role).fingerprint(),
					"network-policy/kept-ls/tenant-isolation": networkPolicyOriginal(np).fingerprint(),
				}
				if withQuota {
					q, _ := store.QuotaSpecByName(nil, "team-ls")
					out["quota//team-ls"] = quotaOriginal(q).fingerprint()
				}
				return out
			}
			found := live()

			_, err := c.apply("", structs.TenantKind, "team-ls", spec)
			must.NoError(t, err)
			obj := waitTenant(c, "team-ls", "ready", tenantReady)
			must.Eq(t, len(keys), len(tenantAdopted(obj)), must.Sprintf("status: %v", obj.Status))
			written := live()
			recorded := intent.Load()
			must.NotNil(t, recorded, must.Sprint("the tenant wrote nothing"))
			for _, key := range keys {
				must.NotEq(t, found[key], written[key], must.Sprintf("the tenant did not write %s", key))
				must.Eq(t, "", tenantRefs(obj)[key].Writing, must.Sprint(key))
				// What the tenant wrote down that it was writing is what
				// the resource holds after the write.
				must.Eq(t, written[key], tenantRefs(recorded)[key].Writing, must.Sprint(key))
			}

			// The status as a lost write leaves it: everything written,
			// nothing said to be; and the spec changed meanwhile.
			must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
				changed := c.get("", structs.TenantKind, "team-ls").Copy()
				changed.Status["resources"] = structs.CopyObjectMap(recorded.Status)["resources"]
				changed.Spec = structs.CopyObjectMap(changed.Spec)
				changed.Spec["namespace"] = map[string]any{"name": "kept-ls", "description": "Team LS, renamed", "meta": map[string]any{"owner": "ls"}}
				changed.Generation++
				_, err := c.s.writeOperatorObject(changed)
				return err
			}), wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))
			obj = waitTenant(c, "team-ls", "ready with the new spec", func(o *structs.CustomObject) bool {
				ns, _ := store.NamespaceByName(nil, "kept-ls")
				return tenantReady(o) && ns != nil && ns.Description == "Team LS, renamed"
			})
			for _, key := range keys {
				ref := tenantRefs(obj)[key]
				must.True(t, ref.Adopted, must.Sprint(key))
				must.Eq(t, "", ref.Writing, must.Sprint(key))
				must.Eq(t, found[key], ref.Original.fingerprint(), must.Sprintf(
					"%s: the tenant took what it wrote itself for what it found: %+v", key, ref.Original))
			}

			must.NoError(t, c.delete("", structs.TenantKind, "team-ls"))
			waitTenantGone(c, "team-ls")
			must.Eq(t, found, live(), must.Sprint("the resources were not given back as the tenant found them"))
		})
	}
}

// A resource a tenant took over and has not written holds nothing of the
// tenant: a tenant deleted before its first write gives it back as it is,
// and does not write what an earlier pass read over a change made since
// (review finding ATM-26b). One that holds what the tenant was writing was
// written by the tenant, and is restored.
func TestTenantOperator_GiveBackBeforeFirstWrite(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newTenantTest(t)
	defer cleanup()
	store := c.s.State()
	hand := tenantByHand{c}

	_, err := c.apply("", structs.TenantKind, "team-gb", map[string]any{"namespace": map[string]any{"description": "GB"}})
	must.NoError(t, err)
	stored := waitGeneration(c, "team-gb")

	hand.namespace("changed-gb", "", map[string]string{"team": "new"})
	hand.namespace("written-gb", "", map[string]string{"owner": "gb"})
	ns, err := store.NamespaceByName(nil, "written-gb")
	must.NoError(t, err)
	original := map[string]any{"description": "as the first pass read it", "meta": map[string]any{"team": "old"}}

	deleting := stored.Copy()
	deleting.DeletionTime = time.Now().UnixNano()
	deleting.Finalizers = []string{structs.OperatorsFinalizer}
	deleting.Status = map[string]any{"resources": []any{
		map[string]any{"type": structs.TenantResourceNamespace, "name": "changed-gb", "adopted": true, "original": original,
			"writing": namespaceOriginal(&structs.Namespace{Description: "GB"}).fingerprint()},
		map[string]any{"type": structs.TenantResourceNamespace, "name": "written-gb", "adopted": true, "original": original,
			"writing": namespaceOriginal(ns).fingerprint()},
	}}
	o := newTenantOperator(c.s, c.s.logger)
	o.finishDeletion(newTenantPass(t, c.s), deleting)

	ns, err = store.NamespaceByName(nil, "changed-gb")
	must.NoError(t, err)
	must.NotNil(t, ns)
	must.Eq(t, "made by hand", ns.Description, must.Sprint("the deleted tenant wrote what it had read over a namespace it never wrote"))
	must.Eq(t, map[string]string{"team": "new"}, ns.Meta)
	mustHaveEvent(t, c, stored, "Released", "changed-gb: the tenant had not written it")

	ns, err = store.NamespaceByName(nil, "written-gb")
	must.NoError(t, err)
	must.NotNil(t, ns)
	must.Eq(t, "as the first pass read it", ns.Description, must.Sprint("the namespace the tenant wrote was not given back as found"))
	must.Eq(t, map[string]string{"team": "old"}, ns.Meta)
}

// An ACL role a tenant took over and changed, renamed by hand later, is given
// back before the tenant makes a role of its own: the role of the ID the
// tenant recorded gets back what the tenant found, under the name it has now.
// The tenant took a role missing under its name for a role to make again and
// forgot what it had taken over: the renamed role kept what the tenant wrote
// into it (review finding ATM-26c).
func TestTenantOperator_AdoptedRoleRenamed(t *testing.T) {
	ci.Parallel(t)

	adopt := func(t *testing.T, c *certTest, tenant string) *structs.ACLRole {
		t.Helper()
		tenantByHand{c}.policyAndRole(tenant + "-dev")
		byHand, err := c.s.State().GetACLRoleByName(nil, tenant+"-dev")
		must.NoError(t, err)
		must.NotNil(t, byHand)
		_, err = c.apply("", structs.TenantKind, tenant, map[string]any{
			"namespace": map[string]any{"description": "Tenant"},
			"roles":     map[string]any{"dev": []any{"submit-job"}},
			"adopt":     []any{"acl-policy", "acl-role"},
		})
		must.NoError(t, err)
		obj := waitGeneration(c, tenant)
		must.SliceContains(t, tenantAdopted(obj), "acl-role//"+tenant+"-dev")
		written, err := c.s.State().GetACLRoleByID(nil, byHand.ID)
		must.NoError(t, err)
		must.NotEq(t, "made by hand", written.Description, must.Sprint("the tenant did not write the role"))
		return byHand
	}
	// givenBack checks the renamed role and the role of the tenant's own.
	givenBack := func(t *testing.T, c *certTest, tenant string, byHand *structs.ACLRole) {
		t.Helper()
		key := "acl-role//" + tenant + "-dev"
		obj := waitTenant(c, tenant, "ready with a role of its own", func(o *structs.CustomObject) bool {
			ref := tenantRefs(o)[key]
			return tenantReady(o) && ref.ID != "" && ref.ID != byHand.ID && !ref.Pending
		})
		ref := tenantRefs(obj)[key]
		must.False(t, ref.Adopted, must.Sprint("the role the tenant made is marked as taken over"))
		must.Nil(t, ref.Original)

		renamed, err := c.s.State().GetACLRoleByID(nil, byHand.ID)
		must.NoError(t, err)
		must.NotNil(t, renamed)
		must.Eq(t, "ops-dev", renamed.Name)
		must.Eq(t, "made by hand", renamed.Description, must.Sprint("the renamed role was not given back: it keeps what the tenant wrote"))
		must.Len(t, 1, renamed.Policies)
		must.Eq(t, tenant+"-dev-other", renamed.Policies[0].Name)
		mustHaveEvent(t, c, obj, "Restored", "gave acl-role "+tenant+"-dev back")

		own, err := c.s.State().GetACLRoleByName(nil, tenant+"-dev")
		must.NoError(t, err)
		must.NotNil(t, own)
		must.Eq(t, ref.ID, own.ID)
		must.Len(t, 1, own.Policies)
		must.Eq(t, tenant+"-dev", own.Policies[0].Name)

		// The role of its own goes with the tenant; the renamed one stays.
		must.NoError(t, c.delete("", structs.TenantKind, tenant))
		waitTenantGone(c, tenant)
		own, err = c.s.State().GetACLRoleByID(nil, ref.ID)
		must.NoError(t, err)
		must.Nil(t, own, must.Sprint("the role the tenant made stayed"))
		renamed, err = c.s.State().GetACLRoleByID(nil, byHand.ID)
		must.NoError(t, err)
		must.NotNil(t, renamed, must.Sprint("the deleted tenant deleted the role it had given back"))
		must.Eq(t, "made by hand", renamed.Description)
	}

	t.Run("renamed", func(t *testing.T) {
		ci.Parallel(t)
		c, cleanup := newTenantTest(t)
		defer cleanup()
		byHand := adopt(t, c, "team-n1")
		renameRole(t, c.s, "team-n1-dev", "ops-dev")
		givenBack(t, c, "team-n1", byHand)
	})

	// Somebody else's role has the name by the time the tenant looks: the
	// renamed role is given back all the same, and the other role is taken
	// over as any role the tenant finds.
	t.Run("renamed and replaced", func(t *testing.T) {
		ci.Parallel(t)
		c, cleanup := newTenantTest(t)
		defer cleanup()
		byHand := adopt(t, c, "team-n2")
		role, err := c.s.State().GetACLRoleByID(nil, byHand.ID)
		must.NoError(t, err)
		renamed := role.Copy()
		renamed.Name = "ops-dev"
		renamed.SetHash()
		other := &structs.ACLRole{ID: uuid.Generate(), Name: "team-n2-dev", Description: "somebody else's",
			Policies: []*structs.ACLRolePolicyLink{{Name: "team-n2-dev-other"}}}
		other.SetHash()
		raftWrite(t, c.s, structs.ACLRolesUpsertRequestType, &structs.ACLRolesUpsertRequest{ACLRoles: []*structs.ACLRole{renamed, other}})

		obj := waitTenant(c, "team-n2", "ready with the other role", func(o *structs.CustomObject) bool {
			return tenantReady(o) && tenantRefs(o)["acl-role//team-n2-dev"].ID == other.ID
		})
		ref := tenantRefs(obj)["acl-role//team-n2-dev"]
		must.True(t, ref.Adopted)
		must.Eq(t, "somebody else's", ref.Original.Description)
		role, err = c.s.State().GetACLRoleByID(nil, byHand.ID)
		must.NoError(t, err)
		must.NotNil(t, role)
		must.Eq(t, "ops-dev", role.Name)
		must.Eq(t, "made by hand", role.Description, must.Sprint("the renamed role was not given back: it keeps what the tenant wrote"))
		must.Len(t, 1, role.Policies)
		must.Eq(t, "team-n2-dev-other", role.Policies[0].Name)
	})

	// A renamed role that can't be given back keeps the tenant from making
	// its own, with the condition RestoreFailed, until it can be.
	t.Run("can't be given back", func(t *testing.T) {
		ci.Parallel(t)
		c, cleanup := newTenantTest(t)
		defer cleanup()
		byHand := adopt(t, c, "team-n3")
		var resp structs.GenericResponse
		must.NoError(t, c.s.RPC("ACL.DeletePolicies", &structs.ACLPolicyDeleteRequest{
			Names: []string{"team-n3-dev-other"}, WriteRequest: c.write(""),
		}, &resp))
		renameRole(t, c.s, "team-n3-dev", "ops-dev")

		obj := waitTenant(c, "team-n3", "RestoreFailed", func(o *structs.CustomObject) bool {
			return objectConditionTrue(o.Status, structs.TenantConditionRestoreFailed)
		})
		must.False(t, tenantReady(obj))
		ref := tenantRefs(obj)["acl-role//team-n3-dev"]
		must.True(t, ref.Adopted, must.Sprint("the tenant forgot the role it could not give back"))
		must.Eq(t, byHand.ID, ref.ID)
		own, err := c.s.State().GetACLRoleByName(nil, "team-n3-dev")
		must.NoError(t, err)
		must.Nil(t, own, must.Sprint("the tenant made its own role before it gave the renamed one back"))
		mustHaveEvent(t, c, obj, "RestoreFailed", "team-n3-dev-other")

		tenantByHand{c}.policy("team-n3-dev-other", `agent { policy = "read" }`)
		givenBack(t, c, "team-n3", byHand)
	})
}

// An ACL role a tenant created, renamed by hand, is renamed back: the role of
// the tenant is the one of the ID it recorded, so the tenant writes that role
// back to its name and to its spec, and makes no second role (review finding
// ATM-26d). The tenant took a role missing under its name for a role to make
// again: it made a new one, and the renamed role stayed behind, unlisted,
// linking the policy of the tenant.
func TestTenantOperator_OwnRoleRenamed(t *testing.T) {
	ci.Parallel(t)

	create := func(t *testing.T, c *certTest, tenant string) *structs.ACLRole {
		t.Helper()
		_, err := c.apply("", structs.TenantKind, tenant, map[string]any{
			"namespace": map[string]any{"description": "Tenant"},
			"roles":     map[string]any{"dev": []any{"submit-job"}},
		})
		must.NoError(t, err)
		obj := waitGeneration(c, tenant)
		own, err := c.s.State().GetACLRoleByName(nil, tenant+"-dev")
		must.NoError(t, err)
		must.NotNil(t, own)
		must.Eq(t, own.ID, tenantRefs(obj)["acl-role//"+tenant+"-dev"].ID)
		return own
	}

	t.Run("renamed", func(t *testing.T) {
		ci.Parallel(t)
		c, cleanup := newTenantTest(t)
		defer cleanup()
		own := create(t, c, "team-o1")
		renameRole(t, c.s, "team-o1-dev", "ops-dev")

		// The tenant renames the role back and records the event in two
		// writes: the role has its name before the event is there, so the
		// wait takes the event too.
		obj := waitTenant(c, "team-o1", "ready with a role of its name, the rename recorded", func(o *structs.CustomObject) bool {
			role, err := c.s.State().GetACLRoleByName(nil, "team-o1-dev")
			return err == nil && role != nil && tenantReady(o) && !tenantRefs(o)["acl-role//team-o1-dev"].Pending &&
				hasObjectEvent(c, o, "Renamed", "renamed acl-role team-o1-dev back: it had the name ops-dev")
		})
		role, err := c.s.State().GetACLRoleByName(nil, "team-o1-dev")
		must.NoError(t, err)
		must.Eq(t, own.ID, role.ID, must.Sprint("the tenant made a second role instead of renaming its role back"))
		must.Eq(t, "team-o1-dev", role.Policies[0].Name)
		left, err := c.s.State().GetACLRoleByName(nil, "ops-dev")
		must.NoError(t, err)
		must.Nil(t, left, must.Sprint("the renamed role of the tenant stayed behind"))
		must.Eq(t, own.ID, tenantRefs(c.get("", structs.TenantKind, "team-o1"))["acl-role//team-o1-dev"].ID)
		mustHaveEvent(t, c, obj, "Renamed", "renamed acl-role team-o1-dev back: it had the name ops-dev")

		must.NoError(t, c.delete("", structs.TenantKind, "team-o1"))
		waitTenantGone(c, "team-o1")
		role, err = c.s.State().GetACLRoleByID(nil, own.ID)
		must.NoError(t, err)
		must.Nil(t, role, must.Sprint("the role of the deleted tenant stayed"))
	})

	// Somebody else's role has the name by the time the tenant looks: the
	// tenant writes neither role and makes none, and keeps its own role, by
	// its ID, in its list; a deleted tenant deletes that one.
	t.Run("renamed and the name taken", func(t *testing.T) {
		ci.Parallel(t)
		c, cleanup := newTenantTest(t)
		defer cleanup()
		tenantByHand{c}.policy("ops-read", `node { policy = "read" }`)
		own := create(t, c, "team-o2")
		renamed := own.Copy()
		renamed.Name = "ops-dev"
		renamed.SetHash()
		other := &structs.ACLRole{ID: uuid.Generate(), Name: "team-o2-dev", Description: "somebody else's",
			Policies: []*structs.ACLRolePolicyLink{{Name: "ops-read"}}}
		other.SetHash()
		raftWrite(t, c.s, structs.ACLRolesUpsertRequestType, &structs.ACLRolesUpsertRequest{ACLRoles: []*structs.ACLRole{renamed, other}})

		obj := waitTenant(c, "team-o2", "in conflict", func(o *structs.CustomObject) bool {
			return objectConditionTrue(o.Status, structs.TenantConditionConflict)
		})
		must.Eq(t, own.ID, tenantRefs(obj)["acl-role//team-o2-dev"].ID,
			must.Sprint("the tenant no longer lists the role it created"))
		conflicts := fmt.Sprint(obj.Status["conflicts"])
		must.StrContains(t, conflicts, `acl-role team-o2-dev has the name "ops-dev" now, and another role has its name`)
		role, err := c.s.State().GetACLRoleByID(nil, own.ID)
		must.NoError(t, err)
		must.NotNil(t, role)
		must.Eq(t, "ops-dev", role.Name)
		role, err = c.s.State().GetACLRoleByName(nil, "team-o2-dev")
		must.NoError(t, err)
		must.NotNil(t, role)
		must.Eq(t, other.ID, role.ID)
		must.Eq(t, "somebody else's", role.Description, must.Sprint("the tenant wrote the role of somebody else"))

		must.NoError(t, c.delete("", structs.TenantKind, "team-o2"))
		waitTenantGone(c, "team-o2")
		role, err = c.s.State().GetACLRoleByID(nil, own.ID)
		must.NoError(t, err)
		must.Nil(t, role, must.Sprint("the role the deleted tenant created stayed, under the name it was given"))
		role, err = c.s.State().GetACLRoleByID(nil, other.ID)
		must.NoError(t, err)
		must.NotNil(t, role, must.Sprint("the deleted tenant deleted the role of somebody else"))
	})
}

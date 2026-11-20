// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
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

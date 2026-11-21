// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// newTenantArmedTest returns a tenant test server whose tenant operator runs
// the function armed with, once, right before it writes the resource of the
// key target: unlike newTenantRaceTest, the function is set when the test
// wants it, not for the first write. Release kinds are registered too.
func newTenantArmedTest(t *testing.T, target string) (*certTest, *atomic.Pointer[func(s *Server)]) {
	t.Helper()
	armed := &atomic.Pointer[func(s *Server)]{}
	var srv atomic.Pointer[Server]
	s, root, cleanup := TestACLServer(t, func(c *Config) {
		c.tenantBeforeWrite = func(ref tenantResourceRef) {
			if ref.key() != target {
				return
			}
			if f := armed.Swap(nil); f != nil {
				(*f)(srv.Load())
			}
		}
	})
	t.Cleanup(cleanup)
	srv.Store(s)
	testutil.WaitForLeader(t, s.RPC)
	waitTenantKind(t, s)
	must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
		return s.releases.ensureKinds()
	}), wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))
	return &certTest{t: t, s: s, token: root.SecretID}, armed
}

// waitGeneration waits until a pass over the current generation of the
// tenant ended with the tenant ready.
func waitGeneration(c *certTest, name string) *structs.CustomObject {
	c.t.Helper()
	return waitTenant(c, name, "ready", tenantReady)
}

// recordReleaseOwner records, past the claims, a release that owns the
// resources of the keys, as an operation of another leader would.
func recordReleaseOwner(t *testing.T, s *Server, name string, refs ...*structs.ReleaseResourceRef) {
	t.Helper()
	_, err := s.releases.writeRelease(&structs.Release{
		ID: uuid.Generate(), Namespace: "default", Name: name,
		Spec:   &structs.ReleaseSpec{MaxHistory: structs.DefaultReleaseMaxHistory, Revision: 1},
		Status: &structs.ReleaseStatus{Phase: structs.ReleasePhaseDeployed, Revision: 1, Resources: refs},
	}, 0)
	must.NoError(t, err)
}

// replaceRole deletes the role of the name and creates another role of the
// name, with another ID: somebody else's, linking the policy ops-read.
func replaceRole(t *testing.T, s *Server, name string) string {
	t.Helper()
	old, err := s.State().GetACLRoleByName(nil, name)
	must.NoError(t, err)
	must.NotNil(t, old)
	raftWrite(t, s, structs.ACLRolesDeleteByIDRequestType, &structs.ACLRolesDeleteByIDRequest{ACLRoleIDs: []string{old.ID}})
	role := &structs.ACLRole{ID: uuid.Generate(), Name: name, Description: "made by hand",
		Policies: []*structs.ACLRolePolicyLink{{Name: "ops-read"}}}
	role.SetHash()
	raftWrite(t, s, structs.ACLRolesUpsertRequestType, &structs.ACLRolesUpsertRequest{ACLRoles: []*structs.ACLRole{role}})
	return role.ID
}

// A tenant deletes its ACL role by the ID it recorded, and keeps a role of
// the name that is not the one it created (review finding ATM-7): somebody
// deleted the tenant's role and created another one of its name. The tenant
// looked the ID up by the name and deleted that role, and a pass that kept
// the role set the other role back to what the tenant wants, as its own.
func TestTenant_RoleByRecordedID(t *testing.T) {
	ci.Parallel(t)

	for _, keep := range []bool{true, false} {
		name := map[bool]string{true: "kept in the spec", false: "dropped from the spec"}[keep]
		t.Run(name, func(t *testing.T) {
			ci.Parallel(t)
			c, armed := newTenantArmedTest(t, "namespace//team-a")
			raftWrite(t, c.s, structs.ACLPolicyUpsertRequestType, &structs.ACLPolicyUpsertRequest{
				Policies: []*structs.ACLPolicy{{Name: "ops-read", Rules: `node { policy = "read" }`}},
			})
			roles := map[string]any{"developer": []any{"submit-job"}}
			_, err := c.apply("", structs.TenantKind, "team-a", map[string]any{
				"namespace": map[string]any{"description": "A1"}, "roles": roles,
			})
			must.NoError(t, err)
			waitGeneration(c, "team-a")

			// While the next pass writes the namespace, after it read the
			// role, somebody replaces the role.
			var foreignID string
			replace := func(s *Server) { foreignID = replaceRole(t, s, "team-a-developer") }
			armed.Store(&replace)
			spec := map[string]any{"namespace": map[string]any{"description": "A2"}}
			if keep {
				spec["roles"] = roles
			}
			_, err = c.apply("", structs.TenantKind, "team-a", spec)
			must.NoError(t, err)

			if keep {
				waitTenant(c, "team-a", "in conflict", func(o *structs.CustomObject) bool {
					return objectConditionTrue(o.Status, structs.TenantConditionConflict)
				})
			} else {
				waitGeneration(c, "team-a")
			}
			must.Nil(t, armed.Load(), must.Sprint("the role was not replaced during the pass"))
			role, err := c.s.State().GetACLRoleByName(nil, "team-a-developer")
			must.NoError(t, err)
			must.NotNil(t, role, must.Sprint("the tenant deleted a role it did not create"))
			must.Eq(t, foreignID, role.ID)
			must.Eq(t, "made by hand", role.Description, must.Sprint("the tenant changed a role it did not create"))
			must.SliceNotContains(t, tenantResources(c.get("", structs.TenantKind, "team-a")), "acl-role//team-a-developer")
		})
	}
}

// A tenant gives an ACL role it took over back by the ID it recorded
// (review finding ATM-14): somebody deleted the role and created another
// role of its name. The tenant's role is gone, and the other role is left as
// it is. The give-back looked the role up by its name and wrote what the
// tenant had found onto the other role.
func TestTenant_RoleGivenBackByRecordedID(t *testing.T) {
	ci.Parallel(t)

	// The role leaves the spec of a living tenant.
	t.Run("dropped from the spec", func(t *testing.T) {
		ci.Parallel(t)
		c, armed := newTenantArmedTest(t, "namespace//team-g")
		raftWrite(t, c.s, structs.ACLPolicyUpsertRequestType, &structs.ACLPolicyUpsertRequest{
			Policies: []*structs.ACLPolicy{{Name: "ops-read", Rules: `node { policy = "read" }`}},
		})
		tenantByHand{c}.policyAndRole("team-g-dev")
		_, err := c.apply("", structs.TenantKind, "team-g", map[string]any{
			"namespace": map[string]any{"description": "G1"},
			"roles":     map[string]any{"dev": []any{"submit-job"}},
			"adopt":     []any{"acl-policy", "acl-role"},
		})
		must.NoError(t, err)
		obj := waitGeneration(c, "team-g")
		must.SliceContains(t, tenantAdopted(obj), "acl-role//team-g-dev")
		must.NotEq(t, "", tenantRefs(obj)["acl-role//team-g-dev"].ID, must.Sprint("the tenant recorded no ID"))

		// While the pass that drops the role writes the namespace, before
		// it gives the role back, somebody replaces the role.
		var foreignID string
		replace := func(s *Server) { foreignID = replaceRole(t, s, "team-g-dev") }
		armed.Store(&replace)
		_, err = c.apply("", structs.TenantKind, "team-g", map[string]any{"namespace": map[string]any{"description": "G2"}})
		must.NoError(t, err)
		obj = waitGeneration(c, "team-g")
		must.Nil(t, armed.Load(), must.Sprint("the role was not replaced during the pass"))

		role, err := c.s.State().GetACLRoleByName(nil, "team-g-dev")
		must.NoError(t, err)
		must.NotNil(t, role, must.Sprint("the role of somebody else is gone"))
		must.Eq(t, foreignID, role.ID)
		must.Len(t, 1, role.Policies)
		must.Eq(t, "ops-read", role.Policies[0].Name, must.Sprint("the tenant gave its role back onto a role it did not take over"))
		must.SliceNotContains(t, tenantResources(obj), "acl-role//team-g-dev")
		mustHaveEvent(t, c, obj, "Released", "team-g-dev: it no longer exists")
	})

	// The tenant is deleted, driven by hand on what it listed.
	t.Run("tenant deleted", func(t *testing.T) {
		ci.Parallel(t)
		c, _ := newTenantArmedTest(t, "")
		raftWrite(t, c.s, structs.ACLPolicyUpsertRequestType, &structs.ACLPolicyUpsertRequest{
			Policies: []*structs.ACLPolicy{{Name: "ops-read", Rules: `node { policy = "read" }`}},
		})
		_, err := c.apply("", structs.TenantKind, "team-h", map[string]any{
			"namespace": map[string]any{"description": "H1"},
		})
		must.NoError(t, err)
		stored := waitGeneration(c, "team-h")

		// The role the tenant took over, of the ID it recorded, is gone;
		// somebody else's role has its name.
		role := &structs.ACLRole{ID: uuid.Generate(), Name: "team-h-dev", Description: "made by hand",
			Policies: []*structs.ACLRolePolicyLink{{Name: "ops-read"}}}
		role.SetHash()
		raftWrite(t, c.s, structs.ACLRolesUpsertRequestType, &structs.ACLRolesUpsertRequest{ACLRoles: []*structs.ACLRole{role}})

		deleting := stored.Copy()
		deleting.DeletionTime = time.Now().UnixNano()
		deleting.Finalizers = []string{structs.OperatorsFinalizer}
		deleting.Status = map[string]any{"resources": []any{
			map[string]any{"type": structs.TenantResourceACLRole, "name": "team-h-dev", "id": uuid.Generate(), "adopted": true,
				"original": map[string]any{"description": "as the tenant found it", "policies": []any{"ops-read"}}},
		}}
		o := newTenantOperator(c.s, c.s.logger)
		o.finishDeletion(newTenantPass(t, c.s), deleting)

		live, err := c.s.State().GetACLRoleByName(nil, "team-h-dev")
		must.NoError(t, err)
		must.NotNil(t, live, must.Sprint("the role of somebody else is gone"))
		must.Eq(t, role.ID, live.ID)
		must.Eq(t, "made by hand", live.Description, must.Sprint("the deleted tenant gave its role back onto a role it did not take over"))
		mustHaveEvent(t, c, stored, "Released", "team-h-dev: it no longer exists")
	})
}

// replacePolicy deletes the ACL policy of the name and creates another
// policy of the name: somebody else's, with the rules it returns.
func replacePolicy(t *testing.T, s *Server, name string) string {
	t.Helper()
	const rules = `agent { policy = "read" }`
	raftWrite(t, s, structs.ACLPolicyDeleteRequestType, &structs.ACLPolicyDeleteRequest{Names: []string{name}})
	policy := &structs.ACLPolicy{Name: name, Description: "made by somebody else", Rules: rules}
	policy.SetHash()
	raftWrite(t, s, structs.ACLPolicyUpsertRequestType, &structs.ACLPolicyUpsertRequest{Policies: []*structs.ACLPolicy{policy}})
	return rules
}

// A resource a tenant took over that somebody deleted and created again
// under its name is not the resource the tenant took over (review finding
// ATM-26e): a namespace, a quota, an ACL policy and a network policy have no
// ID, so the tenant records the create index of what it takes over. The
// tenant treats the other resource as any resource it finds, and never
// writes what it found in the deleted one onto it. It went on as if nothing
// had happened: it wrote the other resource as its own, and gave it "back"
// holding what the deleted one had held.
func TestTenant_AdoptedReplacedUnderItsName(t *testing.T) {
	ci.Parallel(t)

	// The tenant still adopts the type: it takes the other policy over as
	// it is now, and gives that back.
	t.Run("adopt kept", func(t *testing.T) {
		ci.Parallel(t)
		c, armed := newTenantArmedTest(t, "namespace//team-e")
		tenantByHand{c}.policyAndRole("team-e-dev")
		spec := map[string]any{
			"namespace": map[string]any{"description": "E1"},
			"roles":     map[string]any{"dev": []any{"submit-job"}},
			"adopt":     []any{"acl-policy", "acl-role"},
		}
		_, err := c.apply("", structs.TenantKind, "team-e", spec)
		must.NoError(t, err)
		obj := waitGeneration(c, "team-e")
		must.SliceContains(t, tenantAdopted(obj), "acl-policy//team-e-dev")

		// While the next pass writes the namespace, after it read the
		// policy, somebody replaces the policy.
		var foreign string
		replace := func(s *Server) { foreign = replacePolicy(t, s, "team-e-dev") }
		armed.Store(&replace)
		spec["namespace"] = map[string]any{"description": "E2"}
		_, err = c.apply("", structs.TenantKind, "team-e", spec)
		must.NoError(t, err)
		waitTenant(c, "team-e", "the policy written by the tenant again", func(o *structs.CustomObject) bool {
			policy, err := c.s.State().ACLPolicyByName(nil, "team-e-dev")
			return err == nil && policy != nil && strings.Contains(policy.Rules, "submit-job") &&
				armed.Load() == nil && tenantReady(o)
		})
		ref := tenantRefs(c.get("", structs.TenantKind, "team-e"))["acl-policy//team-e-dev"]
		must.True(t, ref.Adopted)
		must.NotNil(t, ref.Original)
		must.Eq(t, foreign, ref.Original.Rules,
			must.Sprint("the tenant kept what the deleted policy held as the original of the policy of somebody else"))

		must.NoError(t, c.delete("", structs.TenantKind, "team-e"))
		waitTenantGone(c, "team-e")
		policy, err := c.s.State().ACLPolicyByName(nil, "team-e-dev")
		must.NoError(t, err)
		must.NotNil(t, policy)
		must.Eq(t, foreign, policy.Rules, must.Sprint("the tenant gave the policy back holding what the deleted policy had held"))
		must.Eq(t, "made by somebody else", policy.Description)
	})

	// The tenant no longer adopts the type: the other policy is a conflict,
	// and stays as it is.
	t.Run("adopt dropped", func(t *testing.T) {
		ci.Parallel(t)
		c, armed := newTenantArmedTest(t, "namespace//team-j")
		tenantByHand{c}.policyAndRole("team-j-dev")
		roles := map[string]any{"dev": []any{"submit-job"}}
		_, err := c.apply("", structs.TenantKind, "team-j", map[string]any{
			"namespace": map[string]any{"description": "J1"}, "roles": roles,
			"adopt": []any{"acl-policy", "acl-role"},
		})
		must.NoError(t, err)
		waitGeneration(c, "team-j")

		var foreign string
		replace := func(s *Server) { foreign = replacePolicy(t, s, "team-j-dev") }
		armed.Store(&replace)
		_, err = c.apply("", structs.TenantKind, "team-j", map[string]any{
			"namespace": map[string]any{"description": "J2"}, "roles": roles,
		})
		must.NoError(t, err)
		obj := waitTenant(c, "team-j", "in conflict", func(o *structs.CustomObject) bool {
			return objectConditionTrue(o.Status, structs.TenantConditionConflict)
		})
		must.Nil(t, armed.Load(), must.Sprint("the policy was not replaced during the pass"))
		must.SliceNotContains(t, tenantResources(obj), "acl-policy//team-j-dev")
		policy, err := c.s.State().ACLPolicyByName(nil, "team-j-dev")
		must.NoError(t, err)
		must.NotNil(t, policy)
		must.Eq(t, foreign, policy.Rules, must.Sprint("the tenant wrote a policy it did not take over"))

		must.NoError(t, c.delete("", structs.TenantKind, "team-j"))
		waitTenantGone(c, "team-j")
		policy, err = c.s.State().ACLPolicyByName(nil, "team-j-dev")
		must.NoError(t, err)
		must.NotNil(t, policy)
		must.Eq(t, foreign, policy.Rules, must.Sprint("the deleted tenant wrote a policy it did not take over"))
	})

	// The tenant is deleted, driven by hand on what it listed: the four
	// types that are known by their name.
	t.Run("tenant deleted", func(t *testing.T) {
		ci.Parallel(t)
		c, _ := newTenantArmedTest(t, "")
		store := c.s.State()
		hand := tenantByHand{c}
		hand.quota("team-k-quota")
		hand.namespace("team-k-ns", "", nil)
		hand.policy("team-k-dev", `node { policy = "read" }`)
		handNP := hand.networkPolicy("team-k-ns")
		_, err := c.apply("", structs.TenantKind, "team-k", map[string]any{
			"namespace": map[string]any{"description": "K1"},
		})
		must.NoError(t, err)
		stored := waitGeneration(c, "team-k")

		ns, _ := store.NamespaceByName(nil, "team-k-ns")
		quota, _ := store.QuotaSpecByName(nil, "team-k-quota")
		policy, _ := store.ACLPolicyByName(nil, "team-k-dev")
		np, _ := store.NetworkPolicyByName(nil, "team-k-ns", structs.TenantNetworkPolicyName)
		was := handNP.Copy()
		was.Description = "as the tenant found it"
		netOriginal, err := structs.EncodeObjectPart(&tenantOriginal{Description: was.Description, NetworkPolicy: was})
		must.NoError(t, err)

		// What the tenant took over had other create indexes: it was
		// deleted, and these were created under its names.
		deleting := stored.Copy()
		deleting.DeletionTime = time.Now().UnixNano()
		deleting.Finalizers = []string{structs.OperatorsFinalizer}
		deleting.Status = map[string]any{"resources": []any{
			map[string]any{"type": structs.TenantResourceNamespace, "name": "team-k-ns", "adopted": true,
				"create_index": ns.CreateIndex - 1, "original": map[string]any{"description": "as the tenant found it"}},
			map[string]any{"type": structs.TenantResourceQuota, "name": "team-k-quota", "adopted": true,
				"create_index": quota.CreateIndex - 1, "original": map[string]any{"description": "as the tenant found it"}},
			map[string]any{"type": structs.TenantResourceACLPolicy, "name": "team-k-dev", "adopted": true,
				"create_index": policy.CreateIndex - 1,
				"original":     map[string]any{"description": "as the tenant found it", "rules": `node { policy = "write" }`}},
			map[string]any{"type": structs.TenantResourceNetworkPolicy, "namespace": "team-k-ns", "name": structs.TenantNetworkPolicyName,
				"adopted": true, "create_index": np.CreateIndex - 1, "original": netOriginal},
		}}
		o := newTenantOperator(c.s, c.s.logger)
		o.finishDeletion(newTenantPass(t, c.s), deleting)

		const wrote = "the deleted tenant gave back onto a resource it did not take over"
		ns, _ = store.NamespaceByName(nil, "team-k-ns")
		must.Eq(t, "made by hand", ns.Description, must.Sprint(wrote))
		quota, _ = store.QuotaSpecByName(nil, "team-k-quota")
		must.Eq(t, "made by hand", quota.Description, must.Sprint(wrote))
		policy, _ = store.ACLPolicyByName(nil, "team-k-dev")
		must.Eq(t, "made by hand", policy.Description, must.Sprint(wrote))
		must.Eq(t, `node { policy = "read" }`, policy.Rules, must.Sprint(wrote))
		np, _ = store.NetworkPolicyByName(nil, "team-k-ns", structs.TenantNetworkPolicyName)
		must.Eq(t, "made by hand", np.Description, must.Sprint(wrote))
		for _, name := range []string{"namespace team-k-ns", "quota team-k-quota", "acl-policy team-k-dev",
			"network-policy team-k-ns/" + structs.TenantNetworkPolicyName} {
			mustHaveEvent(t, c, stored, "Released", name+": it no longer exists")
		}
	})
}

// A tenant does not give an ACL role it took over back under a name that is
// somebody else's now (review finding ATM-26f). The role is the one of the ID
// the tenant recorded and is written under the name it has now (ATM-14); when
// it was renamed, that is not the name the tenant lists and claims, and
// another tenant may have taken the role over under it. The tenant then
// keeps the role in its list (RestoreFailed) and writes nothing. It wrote
// what it had found onto the role of the other tenant.
func TestTenant_RoleNotGivenBackOntoAnotherOwner(t *testing.T) {
	ci.Parallel(t)
	c, _ := newTenantArmedTest(t, "")
	_, err := c.apply("", structs.TenantKind, "team-f", map[string]any{
		"namespace": map[string]any{"description": "F1"},
	})
	must.NoError(t, err)
	stored := waitGeneration(c, "team-f")
	claims := c.s.releases.claims

	cases := []struct {
		name string
		role string
		// owned makes the name of the role somebody else's, and returns
		// what undoes it.
		owned func(p *tenantPass, key string) func()
		want  string
	}{
		{
			name: "of another tenant", role: "ops-a",
			owned: func(p *tenantPass, key string) func() {
				p.tenants[key] = "team-o"
				return func() {}
			},
			want: "acl-role ops-a belongs to the tenant team-o",
		},
		{
			name: "being written", role: "ops-b",
			owned: func(_ *tenantPass, key string) func() {
				other := claims.claimant(claimTenant, "team-o")
				claims.claim(other, []string{key})
				return func() { claims.release(other) }
			},
			want: `acl-role ops-b is being written by tenant "team-o"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The role the tenant took over as team-f-dev has another name
			// now, and links another policy than the tenant found.
			tenantByHand{c}.policyAndRole(tc.role)
			role, err := c.s.State().GetACLRoleByName(nil, tc.role)
			must.NoError(t, err)
			must.NotNil(t, role)

			p := newTenantPass(t, c.s)
			undo := tc.owned(p, tenantResourceRef{Type: structs.TenantResourceACLRole, Name: tc.role}.key())
			defer undo()

			deleting := stored.Copy()
			deleting.DeletionTime = time.Now().UnixNano()
			deleting.Finalizers = []string{structs.OperatorsFinalizer}
			deleting.Status = map[string]any{"resources": []any{
				map[string]any{"type": structs.TenantResourceACLRole, "name": "team-f-dev", "id": role.ID, "adopted": true,
					"original": map[string]any{"description": "as the tenant found it", "policies": []any{tc.role}}},
			}}
			o := newTenantOperator(c.s, c.s.logger)
			o.finishDeletion(p, deleting)

			live, err := c.s.State().GetACLRoleByID(nil, role.ID)
			must.NoError(t, err)
			must.NotNil(t, live)
			must.Eq(t, "made by hand", live.Description, must.Sprint("the tenant gave its role back under a name that is somebody else's"))
			must.Eq(t, tc.role+"-other", live.Policies[0].Name)
			mustHaveEvent(t, c, stored, "RestoreFailed", tc.want)
			must.NotNil(t, c.get("", structs.TenantKind, "team-f"), must.Sprint("the tenant let go of a role it could not give back"))
		})
	}
}

// A tenant does not delete a resource it lists that a release owns, or that
// a release operation is writing (review finding ATM-13): one a release
// owns is dropped from the tenant's list with an event, one being written
// waits. Both delete paths deleted what the tenant listed without asking.
func TestTenant_DeleteLeavesReleaseResource(t *testing.T) {
	ci.Parallel(t)

	// The resources the tenant no longer wants.
	t.Run("no longer wanted", func(t *testing.T) {
		ci.Parallel(t)
		c, armed := newTenantArmedTest(t, "namespace//team-r")
		_, err := c.apply("", structs.TenantKind, "team-r", map[string]any{
			"namespace": map[string]any{"description": "R1"},
			"roles":     map[string]any{"developer": []any{"submit-job"}},
		})
		must.NoError(t, err)
		waitGeneration(c, "team-r")

		// While the pass that drops the role writes the namespace, a
		// release records the policy of the role as its own.
		record := func(s *Server) {
			recordReleaseOwner(t, s, "rel", &structs.ReleaseResourceRef{Type: structs.ReleaseResourceACLPolicy, Name: "team-r-developer"})
		}
		armed.Store(&record)
		_, err = c.apply("", structs.TenantKind, "team-r", map[string]any{"namespace": map[string]any{"description": "R2"}})
		must.NoError(t, err)
		obj := waitGeneration(c, "team-r")
		must.Nil(t, armed.Load(), must.Sprint("the release was not recorded during the pass"))

		policy, err := c.s.State().ACLPolicyByName(nil, "team-r-developer")
		must.NoError(t, err)
		must.NotNil(t, policy, must.Sprint("the tenant deleted a policy a release owns"))
		must.SliceNotContains(t, tenantResources(obj), "acl-policy//team-r-developer")
		role, err := c.s.State().GetACLRoleByName(nil, "team-r-developer")
		must.NoError(t, err)
		must.Nil(t, role, must.Sprint("the tenant's own role was not deleted"))
		mustHaveEvent(t, c, obj, "NotDeleted", "team-r-developer")
	})

	// A deleted tenant.
	t.Run("tenant deleted", func(t *testing.T) {
		ci.Parallel(t)
		c, cleanup := newTenantTest(t)
		defer cleanup()
		must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
			return c.s.releases.ensureKinds()
		}), wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))
		_, err := c.apply("", structs.TenantKind, "team-d", map[string]any{
			"roles": map[string]any{"developer": []any{"submit-job"}, "viewer": []any{"read-logs"}},
		})
		must.NoError(t, err)
		stored := waitGeneration(c, "team-d")

		// A release owns one policy of the tenant, and a release operation
		// is writing the other one.
		recordReleaseOwner(t, c.s, "rel", &structs.ReleaseResourceRef{Type: structs.ReleaseResourceACLPolicy, Name: "team-d-developer"})
		claims := c.s.releases.claims
		op := claims.claimant(claimRelease, "default/other")
		claims.claim(op, []string{"acl-policy//team-d-viewer"})
		defer claims.release(op)

		// The deletion, driven by hand on what the tenant listed.
		deleting := stored.Copy()
		deleting.DeletionTime = time.Now().UnixNano()
		deleting.Finalizers = []string{structs.OperatorsFinalizer}
		deleting.Status = map[string]any{"resources": []any{
			map[string]any{"type": structs.TenantResourceACLPolicy, "name": "team-d-developer"},
			map[string]any{"type": structs.TenantResourceACLPolicy, "name": "team-d-viewer"},
		}}
		o := newTenantOperator(c.s, c.s.logger)
		o.finishDeletion(newTenantPass(t, c.s), deleting)

		for _, name := range []string{"team-d-developer", "team-d-viewer"} {
			policy, err := c.s.State().ACLPolicyByName(nil, name)
			must.NoError(t, err)
			must.NotNil(t, policy, must.Sprintf("the deleted tenant deleted the policy %s of a release", name))
		}
		mustHaveEvent(t, c, stored, "NotDeleted", "team-d-developer")
	})
}

// mustHaveEvent checks that the event log of the object has an event of the
// reason whose message holds what.
// hasObjectEvent reports whether the event log of the object has an event
// of the reason whose message contains what.
func hasObjectEvent(c *certTest, obj *structs.CustomObject, reason, what string) bool {
	log, err := c.s.State().ObjectEventLog(nil, obj.Group, obj.Kind, obj.Namespace, obj.Name)
	if err != nil || log == nil {
		return false
	}
	for _, e := range log.Events {
		if e.Reason == reason && strings.Contains(e.Message, what) {
			return true
		}
	}
	return false
}

func mustHaveEvent(t *testing.T, c *certTest, obj *structs.CustomObject, reason, what string) {
	t.Helper()
	log, err := c.s.State().ObjectEventLog(nil, obj.Group, obj.Kind, obj.Namespace, obj.Name)
	must.NoError(t, err)
	must.NotNil(t, log, must.Sprint("the object has no events"))
	var seen []string
	for _, e := range log.Events {
		if e.Reason == reason && strings.Contains(e.Message, what) {
			return
		}
		seen = append(seen, e.Reason+": "+e.Message)
	}
	t.Fatalf("no event %s about %s: %q", reason, what, seen)
}

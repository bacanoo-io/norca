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

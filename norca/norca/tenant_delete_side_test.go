// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
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

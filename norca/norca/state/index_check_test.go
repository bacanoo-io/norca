// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// indexCheckKind is one of the resources whose upserts take a check-and-set
// (ExpectedModifyIndex): how to write named resources with a description,
// and how to read one back.
type indexCheckKind struct {
	name   string
	key    func(name string) string
	upsert func(s *StateStore, index uint64, names []string, desc string, expected map[string]uint64) error
	// read returns the description and the modify index of a resource,
	// "" and 0 when it does not exist.
	read func(t *testing.T, s *StateStore, name string) (string, uint64)
}

func indexCheckKinds() []indexCheckKind {
	plainKey := func(name string) string { return name }
	return []indexCheckKind{
		{
			name: "namespace",
			key:  plainKey,
			upsert: func(s *StateStore, index uint64, names []string, desc string, expected map[string]uint64) error {
				var out []*structs.Namespace
				for _, name := range names {
					out = append(out, &structs.Namespace{Name: name, Description: desc})
				}
				return s.UpsertNamespacesExpected(index, out, expected)
			},
			read: func(t *testing.T, s *StateStore, name string) (string, uint64) {
				ns, err := s.NamespaceByName(nil, name)
				must.NoError(t, err)
				if ns == nil {
					return "", 0
				}
				return ns.Description, ns.ModifyIndex
			},
		},
		{
			name: "quota",
			key:  plainKey,
			upsert: func(s *StateStore, index uint64, names []string, desc string, expected map[string]uint64) error {
				var out []*structs.QuotaSpec
				for _, name := range names {
					out = append(out, &structs.QuotaSpec{Name: name, Description: desc, Limits: []*structs.QuotaLimit{
						{Region: "global", RegionLimit: &structs.Resources{CPU: 100}},
					}})
				}
				return s.UpsertQuotaSpecsExpected(structs.MsgTypeTestSetup, index, out, expected)
			},
			read: func(t *testing.T, s *StateStore, name string) (string, uint64) {
				q, err := s.QuotaSpecByName(nil, name)
				must.NoError(t, err)
				if q == nil {
					return "", 0
				}
				return q.Description, q.ModifyIndex
			},
		},
		{
			name: "ACL policy",
			key:  plainKey,
			upsert: func(s *StateStore, index uint64, names []string, desc string, expected map[string]uint64) error {
				var out []*structs.ACLPolicy
				for _, name := range names {
					out = append(out, &structs.ACLPolicy{Name: name, Description: desc, Rules: `node { policy = "read" }`})
				}
				return s.UpsertACLPoliciesExpected(structs.MsgTypeTestSetup, index, out, expected)
			},
			read: func(t *testing.T, s *StateStore, name string) (string, uint64) {
				p, err := s.ACLPolicyByName(nil, name)
				must.NoError(t, err)
				if p == nil {
					return "", 0
				}
				return p.Description, p.ModifyIndex
			},
		},
		{
			// A role is written by its ID: the stored role of the name is
			// changed, a new one gets a new ID.
			name: "ACL role",
			key:  plainKey,
			upsert: func(s *StateStore, index uint64, names []string, desc string, expected map[string]uint64) error {
				var out []*structs.ACLRole
				for _, name := range names {
					role := &structs.ACLRole{ID: uuid.Generate(), Name: name, Description: desc,
						Policies: []*structs.ACLRolePolicyLink{{Name: "any"}}}
					stored, err := s.GetACLRoleByName(nil, name)
					if err != nil {
						return err
					}
					if stored != nil {
						role.ID = stored.ID
					}
					out = append(out, role)
				}
				return s.UpsertACLRolesExpected(structs.MsgTypeTestSetup, index, out, true, expected)
			},
			read: func(t *testing.T, s *StateStore, name string) (string, uint64) {
				r, err := s.GetACLRoleByName(nil, name)
				must.NoError(t, err)
				if r == nil {
					return "", 0
				}
				return r.Description, r.ModifyIndex
			},
		},
		{
			name: "network policy",
			key:  func(name string) string { return structs.NetworkPolicyIndexKey("default", name) },
			upsert: func(s *StateStore, index uint64, names []string, desc string, expected map[string]uint64) error {
				var out []*structs.NetworkPolicy
				for _, name := range names {
					out = append(out, &structs.NetworkPolicy{Name: name, Namespace: "default", Description: desc,
						DefaultDeny: []string{structs.NetworkPolicyDirectionEgress}})
				}
				return s.UpsertNetworkPoliciesExpected(structs.MsgTypeTestSetup, index, out, expected)
			},
			read: func(t *testing.T, s *StateStore, name string) (string, uint64) {
				p, err := s.NetworkPolicyByName(nil, "default", name)
				must.NoError(t, err)
				if p == nil {
					return "", 0
				}
				return p.Description, p.ModifyIndex
			},
		},
	}
}

// The upserts of namespaces, quotas, ACL policies, ACL roles and network
// policies check ExpectedModifyIndex in their transaction: 0 creates only
// what does not exist, an index changes only what still has it, a resource
// without an entry is written unchecked, and one failed check writes nothing
// of the request (review findings ATM-1, ATM-26).
func TestStateStore_UpsertExpectedModifyIndex(t *testing.T) {
	ci.Parallel(t)

	for _, kind := range indexCheckKinds() {
		t.Run(kind.name, func(t *testing.T) {
			ci.Parallel(t)
			s := testStateStore(t)
			a, b := kind.key("a"), kind.key("b")

			// 0: created while it does not exist.
			must.NoError(t, kind.upsert(s, 1000, []string{"a"}, "one", map[string]uint64{a: 0}))
			desc, index := kind.read(t, s, "a")
			must.Eq(t, "one", desc)
			must.Eq(t, 1000, index)

			// 0 again: it exists now.
			err := kind.upsert(s, 1001, []string{"a"}, "two", map[string]uint64{a: 0})
			must.True(t, structs.IsIndexConflictError(err), must.Sprintf("not a conflict: %v", err))
			must.StrContains(t, err.Error(), "exists (modify index 1000), expected it not to exist")
			desc, _ = kind.read(t, s, "a")
			must.Eq(t, "one", desc)

			// An index that is not the stored one.
			err = kind.upsert(s, 1002, []string{"a"}, "two", map[string]uint64{a: 999})
			must.True(t, structs.IsIndexConflictError(err), must.Sprintf("not a conflict: %v", err))
			must.StrContains(t, err.Error(), "has modify index 1000, expected 999")
			desc, _ = kind.read(t, s, "a")
			must.Eq(t, "one", desc)

			// An index for a resource that does not exist.
			err = kind.upsert(s, 1003, []string{"b"}, "two", map[string]uint64{b: 5})
			must.True(t, structs.IsIndexConflictError(err), must.Sprintf("not a conflict: %v", err))
			must.StrContains(t, err.Error(), "does not exist, expected modify index 5")
			desc, _ = kind.read(t, s, "b")
			must.Eq(t, "", desc)

			// The stored index: changed.
			must.NoError(t, kind.upsert(s, 1004, []string{"a"}, "two", map[string]uint64{a: 1000}))
			desc, index = kind.read(t, s, "a")
			must.Eq(t, "two", desc)
			must.Eq(t, 1004, index)

			// One failed check writes nothing of the request, not even
			// the resource without an entry.
			err = kind.upsert(s, 1005, []string{"b", "a"}, "three", map[string]uint64{a: 0})
			must.True(t, structs.IsIndexConflictError(err), must.Sprintf("not a conflict: %v", err))
			desc, _ = kind.read(t, s, "b")
			must.Eq(t, "", desc)
			desc, _ = kind.read(t, s, "a")
			must.Eq(t, "two", desc)

			// Without an entry, or without a map: written unchecked.
			must.NoError(t, kind.upsert(s, 1006, []string{"a", "b"}, "four", map[string]uint64{b: 0}))
			desc, _ = kind.read(t, s, "a")
			must.Eq(t, "four", desc)
			must.NoError(t, kind.upsert(s, 1007, []string{"a", "b"}, "five", nil))
			desc, index = kind.read(t, s, "b")
			must.Eq(t, "five", desc)
			must.Eq(t, 1007, index)
		})
	}
}

// The check-and-set of an ACL role upsert is keyed by the name of the role in
// the request and made against the stored role of its ID: a rename is checked
// against the role it renames, and a role whose ID no role has is checked
// against the role that has its name (review finding ATM-26).
func TestStateStore_UpsertACLRolesExpected_ByID(t *testing.T) {
	ci.Parallel(t)
	s := testStateStore(t)
	links := []*structs.ACLRolePolicyLink{{Name: "any"}}
	upsert := func(index uint64, role *structs.ACLRole, expected uint64) error {
		return s.UpsertACLRolesExpected(structs.MsgTypeTestSetup, index, []*structs.ACLRole{role}, true,
			map[string]uint64{role.Name: expected})
	}
	conflict := func(t *testing.T, err error, text string) {
		t.Helper()
		must.True(t, structs.IsIndexConflictError(err), must.Sprintf("not a conflict: %v", err))
		must.StrContains(t, err.Error(), text)
	}

	id := uuid.Generate()
	must.NoError(t, upsert(1000, &structs.ACLRole{ID: id, Name: "dev", Description: "one", Policies: links}, 0))

	// A rename with a stale index, and with the index of the role.
	err := upsert(1001, &structs.ACLRole{ID: id, Name: "developer", Description: "one", Policies: links}, 999)
	conflict(t, err, `ACL role "developer" has modify index 1000, expected 999`)
	must.NoError(t, upsert(1002, &structs.ACLRole{ID: id, Name: "developer", Description: "one", Policies: links}, 1000))
	role, err := s.GetACLRoleByID(nil, id)
	must.NoError(t, err)
	must.Eq(t, "developer", role.Name)
	must.Eq(t, 1002, role.ModifyIndex)

	// Another role of the name: not created, with or without an index.
	err = upsert(1003, &structs.ACLRole{ID: uuid.Generate(), Name: "developer", Description: "two", Policies: links}, 0)
	conflict(t, err, `ACL role "developer" exists (modify index 1002), expected it not to exist`)
	err = upsert(1004, &structs.ACLRole{ID: uuid.Generate(), Name: "developer", Description: "two", Policies: links}, 1000)
	conflict(t, err, `ACL role "developer" has modify index 1002, expected 1000`)

	// The role of the ID is gone.
	must.NoError(t, s.DeleteACLRolesByID(structs.MsgTypeTestSetup, 1005, []string{id}))
	err = upsert(1006, &structs.ACLRole{ID: id, Name: "developer", Description: "two", Policies: links}, 1002)
	conflict(t, err, `ACL role "developer" does not exist, expected modify index 1002`)
	role, err = s.GetACLRoleByName(nil, "developer")
	must.NoError(t, err)
	must.Nil(t, role)
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
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

// The upserts of namespaces, quotas, ACL policies and network policies check
// ExpectedModifyIndex in their transaction: 0 creates only what does not
// exist, an index changes only what still has it, a resource without an
// entry is written unchecked, and one failed check writes nothing of the
// request (review finding ATM-1).
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

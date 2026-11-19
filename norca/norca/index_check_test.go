// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
)

// endpointIndexKind is one of the resources whose upserts take a
// check-and-set (ExpectedModifyIndex), as its endpoint and the FSM see it.
type endpointIndexKind struct {
	name    string
	method  string
	msgType structs.MessageType
	key     func(name string) string
	// request returns an upsert of the named resources with a
	// description.
	request func(names []string, desc string, expected map[string]uint64, wr structs.WriteRequest) any
	// read returns the description and the modify index of a resource,
	// "" and 0 when it does not exist.
	read func(t *testing.T, s *Server, name string) (string, uint64)
}

func endpointIndexKinds() []endpointIndexKind {
	plainKey := func(name string) string { return name }
	return []endpointIndexKind{
		{
			name: "namespace", method: "Namespace.UpsertNamespaces", msgType: structs.NamespaceUpsertRequestType, key: plainKey,
			request: func(names []string, desc string, expected map[string]uint64, wr structs.WriteRequest) any {
				req := &structs.NamespaceUpsertRequest{ExpectedModifyIndex: expected, WriteRequest: wr}
				for _, name := range names {
					req.Namespaces = append(req.Namespaces, &structs.Namespace{Name: name, Description: desc})
				}
				return req
			},
			read: func(t *testing.T, s *Server, name string) (string, uint64) {
				ns, err := s.State().NamespaceByName(nil, name)
				must.NoError(t, err)
				if ns == nil {
					return "", 0
				}
				return ns.Description, ns.ModifyIndex
			},
		},
		{
			name: "quota", method: "Quota.UpsertQuotaSpecs", msgType: structs.QuotaSpecUpsertRequestType | structs.IgnoreUnknownTypeFlag, key: plainKey,
			request: func(names []string, desc string, expected map[string]uint64, wr structs.WriteRequest) any {
				req := &structs.QuotaSpecUpsertRequest{ExpectedModifyIndex: expected, WriteRequest: wr}
				for _, name := range names {
					req.Quotas = append(req.Quotas, &structs.QuotaSpec{Name: name, Description: desc, Limits: []*structs.QuotaLimit{
						{Region: "global", RegionLimit: &structs.Resources{CPU: 100}},
					}})
				}
				return req
			},
			read: func(t *testing.T, s *Server, name string) (string, uint64) {
				q, err := s.State().QuotaSpecByName(nil, name)
				must.NoError(t, err)
				if q == nil {
					return "", 0
				}
				return q.Description, q.ModifyIndex
			},
		},
		{
			name: "ACL policy", method: "ACL.UpsertPolicies", msgType: structs.ACLPolicyUpsertRequestType, key: plainKey,
			request: func(names []string, desc string, expected map[string]uint64, wr structs.WriteRequest) any {
				req := &structs.ACLPolicyUpsertRequest{ExpectedModifyIndex: expected, WriteRequest: wr}
				for _, name := range names {
					req.Policies = append(req.Policies, &structs.ACLPolicy{Name: name, Description: desc, Rules: `node { policy = "read" }`})
				}
				return req
			},
			read: func(t *testing.T, s *Server, name string) (string, uint64) {
				p, err := s.State().ACLPolicyByName(nil, name)
				must.NoError(t, err)
				if p == nil {
					return "", 0
				}
				return p.Description, p.ModifyIndex
			},
		},
		{
			name: "network policy", method: "NetworkPolicy.UpsertNetworkPolicies",
			msgType: structs.NetworkPolicyUpsertRequestType | structs.IgnoreUnknownTypeFlag,
			key:     func(name string) string { return structs.NetworkPolicyIndexKey("default", name) },
			request: func(names []string, desc string, expected map[string]uint64, wr structs.WriteRequest) any {
				req := &structs.NetworkPolicyUpsertRequest{ExpectedModifyIndex: expected, WriteRequest: wr}
				for _, name := range names {
					policy := testNetworkPolicy("default", name, nil)
					policy.Description = desc
					req.NetworkPolicies = append(req.NetworkPolicies, policy)
				}
				return req
			},
			read: func(t *testing.T, s *Server, name string) (string, uint64) {
				p, err := s.State().NetworkPolicyByName(nil, "default", name)
				must.NoError(t, err)
				if p == nil {
					return "", 0
				}
				return p.Description, p.ModifyIndex
			},
		},
	}
}

// The upsert endpoints of namespaces, quotas, ACL policies and network
// policies take an expected modify index per resource: 0 creates only what
// does not exist, an index changes only what still has it, a resource
// without an entry is written unchecked, and a refused check is a 409 that
// names the resource. The FSM checks again, for a write that passed the
// endpoint and found the state changed when it was applied (review finding
// ATM-1).
func TestUpsertEndpoints_ExpectedModifyIndex(t *testing.T) {
	ci.Parallel(t)
	s, root, cleanup := TestACLServer(t, nil)
	t.Cleanup(cleanup)
	testutil.WaitForLeader(t, s.RPC)
	wr := structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: root.SecretID}

	for _, kind := range endpointIndexKinds() {
		t.Run(kind.name, func(t *testing.T) {
			upsert := func(names []string, desc string, expected map[string]uint64) error {
				var resp structs.GenericResponse
				return s.RPC(kind.method, kind.request(names, desc, expected, wr), &resp)
			}
			conflict := func(t *testing.T, err error, text string) {
				t.Helper()
				must.True(t, structs.IsIndexConflictError(err), must.Sprintf("not a conflict: %v", err))
				must.StrContains(t, err.Error(), text)
			}
			a, b := kind.key("cas-a"), kind.key("cas-b")

			// 0: created while it does not exist.
			must.NoError(t, upsert([]string{"cas-a"}, "one", map[string]uint64{a: 0}))
			desc, index := kind.read(t, s, "cas-a")
			must.Eq(t, "one", desc)

			// 0 again: it exists now.
			err := upsert([]string{"cas-a"}, "two", map[string]uint64{a: 0})
			conflict(t, err, kind.name+` "`+a+`" exists`)
			desc, _ = kind.read(t, s, "cas-a")
			must.Eq(t, "one", desc)

			// An index that is not the stored one.
			err = upsert([]string{"cas-a"}, "two", map[string]uint64{a: index + 100})
			conflict(t, err, "expected")
			desc, _ = kind.read(t, s, "cas-a")
			must.Eq(t, "one", desc)

			// The stored index: changed.
			must.NoError(t, upsert([]string{"cas-a"}, "two", map[string]uint64{a: index}))
			desc, _ = kind.read(t, s, "cas-a")
			must.Eq(t, "two", desc)

			// One failed check writes nothing of the request.
			err = upsert([]string{"cas-b", "cas-a"}, "three", map[string]uint64{a: index})
			conflict(t, err, "expected")
			desc, _ = kind.read(t, s, "cas-b")
			must.Eq(t, "", desc)

			// An index for a resource the request does not write is a
			// mistake of the caller.
			err = upsert([]string{"cas-a"}, "three", map[string]uint64{kind.key("other"): 0})
			must.ErrorContains(t, err, "which the request does not write")
			must.False(t, structs.IsIndexConflictError(err))

			// Without an entry, or without a map: written unchecked.
			must.NoError(t, upsert([]string{"cas-a", "cas-b"}, "four", map[string]uint64{b: 0}))
			desc, _ = kind.read(t, s, "cas-a")
			must.Eq(t, "four", desc)
			must.NoError(t, upsert([]string{"cas-a", "cas-b"}, "five", nil))
			desc, index = kind.read(t, s, "cas-b")
			must.Eq(t, "five", desc)

			// The FSM checks what reaches it past the endpoint.
			_, _, err = s.raftApply(kind.msgType, kind.request([]string{"cas-b"}, "six", map[string]uint64{b: index - 1}, wr))
			conflict(t, err, "expected")
			desc, _ = kind.read(t, s, "cas-b")
			must.Eq(t, "five", desc)
			_, _, err = s.raftApply(kind.msgType, kind.request([]string{"cas-b"}, "six", map[string]uint64{b: index}, wr))
			must.NoError(t, err)
			desc, _ = kind.read(t, s, "cas-b")
			must.Eq(t, "six", desc)
		})
	}
}

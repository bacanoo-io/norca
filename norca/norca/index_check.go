// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"net/http"
	"slices"

	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// checkExpectedIndexes is the endpoint side of the check-and-set of an
// upsert of namespaces, quotas, ACL policies or network policies
// (structs.CheckModifyIndex). keys are the keys of the resources of the
// request; every key of expected must be one of them. lookup reads a
// resource from a snapshot of the state: whether it exists and its modify
// index. A resource that fails the check is answered with a 409 Conflict
// without a Raft write; the FSM checks again when it applies the write.
func checkExpectedIndexes(kind string, expected map[string]uint64, keys []string,
	lookup func(key string) (bool, uint64, error)) error {
	for key := range expected {
		if !slices.Contains(keys, key) {
			return structs.NewErrRPCCodedf(http.StatusBadRequest,
				"an expected modify index is given for the %s %q, which the request does not write", kind, key)
		}
	}
	for _, key := range keys {
		if _, checked := expected[key]; !checked {
			continue
		}
		exists, have, err := lookup(key)
		if err != nil {
			return err
		}
		if err := structs.CheckModifyIndex(expected, kind, key, exists, have); err != nil {
			return err
		}
	}
	return nil
}

// checkNamespaceIndexes checks the check-and-set of a namespace upsert.
func (s *Server) checkNamespaceIndexes(args *structs.NamespaceUpsertRequest) error {
	if len(args.ExpectedModifyIndex) == 0 {
		return nil
	}
	snap, err := s.fsm.State().Snapshot()
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(args.Namespaces))
	for _, ns := range args.Namespaces {
		keys = append(keys, ns.Name)
	}
	return checkExpectedIndexes("namespace", args.ExpectedModifyIndex, keys, func(name string) (bool, uint64, error) {
		ns, err := snap.NamespaceByName(nil, name)
		if err != nil || ns == nil {
			return false, 0, err
		}
		return true, ns.ModifyIndex, nil
	})
}

// checkQuotaIndexes checks the check-and-set of a quota upsert.
func (s *Server) checkQuotaIndexes(args *structs.QuotaSpecUpsertRequest) error {
	if len(args.ExpectedModifyIndex) == 0 {
		return nil
	}
	snap, err := s.fsm.State().Snapshot()
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(args.Quotas))
	for _, spec := range args.Quotas {
		keys = append(keys, spec.Name)
	}
	return checkExpectedIndexes("quota", args.ExpectedModifyIndex, keys, func(name string) (bool, uint64, error) {
		spec, err := snap.QuotaSpecByName(nil, name)
		if err != nil || spec == nil {
			return false, 0, err
		}
		return true, spec.ModifyIndex, nil
	})
}

// checkACLPolicyIndexes checks the check-and-set of an ACL policy upsert.
func (s *Server) checkACLPolicyIndexes(args *structs.ACLPolicyUpsertRequest) error {
	if len(args.ExpectedModifyIndex) == 0 {
		return nil
	}
	snap, err := s.fsm.State().Snapshot()
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(args.Policies))
	for _, policy := range args.Policies {
		keys = append(keys, policy.Name)
	}
	return checkExpectedIndexes("ACL policy", args.ExpectedModifyIndex, keys, func(name string) (bool, uint64, error) {
		policy, err := snap.ACLPolicyByName(nil, name)
		if err != nil || policy == nil {
			return false, 0, err
		}
		return true, policy.ModifyIndex, nil
	})
}

// checkNetworkPolicyIndexes checks the check-and-set of a network policy
// upsert whose policies have their namespaces set, on snap.
func checkNetworkPolicyIndexes(snap *state.StateSnapshot, args *structs.NetworkPolicyUpsertRequest) error {
	if len(args.ExpectedModifyIndex) == 0 {
		return nil
	}
	keys := make([]string, 0, len(args.NetworkPolicies))
	byKey := make(map[string]*structs.NetworkPolicy, len(args.NetworkPolicies))
	for _, policy := range args.NetworkPolicies {
		key := structs.NetworkPolicyIndexKey(policy.Namespace, policy.Name)
		keys = append(keys, key)
		byKey[key] = policy
	}
	return checkExpectedIndexes("network policy", args.ExpectedModifyIndex, keys, func(key string) (bool, uint64, error) {
		policy, err := snap.NetworkPolicyByName(nil, byKey[key].Namespace, byKey[key].Name)
		if err != nil || policy == nil {
			return false, 0, err
		}
		return true, policy.ModifyIndex, nil
	})
}

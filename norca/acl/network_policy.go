// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package acl

import "fmt"

// NetworkPolicyRule is the namespace level ACL rule of network policies:
//
//	namespace "prod" {
//	  network_policy {
//	    policy = "write" # or "read"
//	  }
//	}
//
// It expands to the read-network-policy and write-network-policy namespace
// capabilities. Admin network policies are cluster objects and require the
// operator read or write rule instead.
type NetworkPolicyRule struct {
	Policy string
}

// parseNetworkPolicyRule validates the network_policy block of a namespace
// policy and adds the capabilities it grants.
func parseNetworkPolicyRule(ns *NamespacePolicy) error {
	if ns.NetworkPolicy == nil {
		return nil
	}
	switch ns.NetworkPolicy.Policy {
	case PolicyRead:
		ns.Capabilities = append(ns.Capabilities, NamespaceCapabilityReadNetworkPolicy)
	case PolicyWrite:
		ns.Capabilities = append(ns.Capabilities,
			NamespaceCapabilityReadNetworkPolicy, NamespaceCapabilityWriteNetworkPolicy)
	default:
		return fmt.Errorf("Invalid network_policy policy %q in namespace %s, must be %q or %q",
			ns.NetworkPolicy.Policy, ns.Name, PolicyRead, PolicyWrite)
	}
	return nil
}

// AllowNetworkPolicyRead checks if the network policies of the namespace
// may be read.
func (a *ACL) AllowNetworkPolicyRead(ns string) bool {
	return a.AllowNsOp(ns, NamespaceCapabilityReadNetworkPolicy)
}

// AllowNetworkPolicyWrite checks if the network policies of the namespace
// may be created, updated and deleted.
func (a *ACL) AllowNetworkPolicyWrite(ns string) bool {
	return a.AllowNsOp(ns, NamespaceCapabilityWriteNetworkPolicy)
}

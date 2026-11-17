// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

const (
	// TenantKind is the kind of tenants: one object for a namespace with its
	// quota, ACL policies and roles, network isolation and what it may ask
	// certificates for. Cluster scoped, in the group of the built-in
	// operators.
	TenantKind = "Tenant"

	// Conditions of a tenant, besides Ready and Degraded.
	TenantConditionConflict          = "Conflict"
	TenantConditionNamespaceNotEmpty = "NamespaceNotEmpty"

	// TenantConditionRestoreFailed is true while a resource the tenant took
	// over can't be given back as the tenant found it.
	TenantConditionRestoreFailed = "RestoreFailed"

	// The types of the resources a tenant keeps, as its status lists them
	// and as spec.adopt names them.
	TenantResourceNamespace     = "namespace"
	TenantResourceQuota         = "quota"
	TenantResourceACLPolicy     = "acl-policy"
	TenantResourceACLRole       = "acl-role"
	TenantResourceNetworkPolicy = "network-policy"

	// TenantNetworkPolicyName is the name of the network policy that isolates
	// the namespace of a tenant.
	TenantNetworkPolicyName = "tenant-isolation"

	// TenantIsolationNone and TenantIsolationNamespace are the values of
	// spec.network.isolation.
	TenantIsolationNone      = "none"
	TenantIsolationNamespace = "namespace"
)

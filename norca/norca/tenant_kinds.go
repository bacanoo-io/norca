// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The kind Tenant: one cluster scoped object that keeps a namespace with
// its quota, the ACL policies and roles of its people, the network rule that
// isolates it, and what it may ask certificates for. The servers create and
// keep these resources (tenant_operator.go); the object lists what it
// created in its status, because the resources have no owner field.
//
// Object events are published as they are: nothing in the spec or the status
// is secret. A role's policy is an ACL policy, which is not secret either.

// tenantKindSchema is the JSON Schema of tenants. The status is written by
// the servers only.
const tenantKindSchema = `{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "properties": {
        "namespace": {
          "type": "object",
          "properties": {
            "name": {"type": "string", "minLength": 1, "maxLength": 128},
            "description": {"type": "string", "maxLength": 256},
            "meta": {"type": "object", "additionalProperties": {"type": "string"}}
          },
          "additionalProperties": false
        },
        "quota": {
          "type": "object",
          "properties": {
            "description": {"type": "string"},
            "enforce": {"type": "string", "enum": ["schedule", "submit"]},
            "region": {
              "type": "object",
              "additionalProperties": {
                "type": "object",
                "properties": {
                  "cpu": {"type": "integer"},
                  "memory": {"type": "integer"},
                  "memory_max": {"type": "integer"},
                  "allocations": {"type": "integer"},
                  "variables_mb": {"type": "integer"}
                },
                "additionalProperties": false
              }
            }
          },
          "additionalProperties": false
        },
        "roles": {"type": "object"},
        "network": {
          "type": "object",
          "properties": {
            "isolation": {"type": "string", "enum": ["none", "namespace"], "default": "none"},
            "allow_from_namespaces": {"type": "array", "maxItems": 64, "items": {"type": "string", "minLength": 1}},
            "allow_from_cidrs": {"type": "array", "maxItems": 256, "items": {"type": "string", "minLength": 1}},
            "required": {"type": "boolean"}
          },
          "additionalProperties": false
        },
        "certificate_dns_suffixes": {"type": "array", "maxItems": 100, "items": {"type": "string", "minLength": 1, "maxLength": 253}},
        "certificate_ip_addresses": {"type": "array", "maxItems": 100, "items": {"type": "string", "minLength": 1}},
        "certificate_issuers": {"type": "array", "maxItems": 100, "items": {"type": "string", "minLength": 1}},
        "dns_suffixes": {"type": "array", "maxItems": 100, "items": {"type": "string", "minLength": 1, "maxLength": 253}},
        "adopt": {"type": "array", "items": {"type": "string", "enum": ["namespace", "quota", "acl-policy", "acl-role", "network-policy"]}},
        "release_without_restore": {"type": "boolean"}
      },
      "additionalProperties": false
    },
    "status": {
      "type": "object",
      "properties": {
        "namespace": {"type": "string"},
        "resources": {
          "type": "array",
          "items": {
            "type": "object",
            "properties": {
              "type": {"type": "string"},
              "name": {"type": "string"},
              "namespace": {"type": "string"},
              "id": {"type": "string"},
              "pending": {"type": "boolean"},
              "creating": {"type": "string"},
              "adopted": {"type": "boolean"},
              "original": {"type": "object"}
            },
            "additionalProperties": false
          }
        },
        "conflicts": {"type": "array", "items": {"type": "string"}},
        "conditions": {"type": "array", "items": {"type": "object"}},
        "observedGeneration": {"type": "integer", "minimum": 0}
      },
      "additionalProperties": false
    }
  }
}`

// builtinTenantKinds returns the kind of tenants.
func builtinTenantKinds() []*structs.ObjectKind {
	return []*structs.ObjectKind{{
		Group:       structs.OperatorsGroup,
		Kind:        structs.TenantKind,
		Description: "A namespace with its quota, ACL policies and roles, network isolation and certificate rights, kept by the servers (built in)",
		Namespaced:  false,
		Schema:      tenantKindSchema,
		Columns: []*structs.ObjectKindColumn{
			{Name: "Namespace", Path: "status.namespace"},
			{Name: "Isolation", Path: "spec.network.isolation"},
		},
	}}
}

// isTenantKind reports whether the kind is the kind of tenants.
func isTenantKind(group, kind string) bool {
	return group == structs.OperatorsGroup && kind == structs.TenantKind
}

// tenantNameRule is what the name of a tenant and of a role may be: the
// names of the namespace, the quota, the ACL policies and roles derive from
// them.
var tenantNameRule = regexp.MustCompile("^[a-zA-Z0-9-]{1,63}$")

// tenantSpec is the spec of a Tenant object.
type tenantSpec struct {
	Namespace struct {
		Name        string            `json:"name"`
		Description string            `json:"description"`
		Meta        map[string]string `json:"meta"`
	} `json:"namespace"`
	Quota *struct {
		Description string                       `json:"description"`
		Enforce     string                       `json:"enforce"`
		Region      map[string]*tenantQuotaLimit `json:"region"`
	} `json:"quota"`
	Roles   map[string]any `json:"roles"`
	Network struct {
		Isolation           string   `json:"isolation"`
		AllowFromNamespaces []string `json:"allow_from_namespaces"`

		// AllowFromCIDRs lets in traffic from these prefixes (TEN-11): the
		// namespace peers match allocations on the same node only, so the
		// instances of the tenant on other nodes and a load balancer, which
		// arrive with the address of a node, need the node subnet or the
		// addresses of the load balancer here.
		AllowFromCIDRs []string `json:"allow_from_cidrs"`

		// Required false places the groups on nodes that can't enforce the
		// isolation too (host networking, nodes without nftables), with
		// warnings: the enforcement best_effort of the network policy. Unset
		// or true is required.
		Required *bool `json:"required"`
	} `json:"network"`
	CertificateDNSSuffixes []string `json:"certificate_dns_suffixes"`
	CertificateIPAddresses []string `json:"certificate_ip_addresses"`
	CertificateIssuers     []string `json:"certificate_issuers"`
	DNSSuffixes            []string `json:"dns_suffixes"`
	Adopt                  []string `json:"adopt"`

	// ReleaseWithoutRestore releases the resources the tenant took over as
	// they are, when the tenant lets them go, instead of writing back what
	// it found: the way out when one can't be given back as it was.
	ReleaseWithoutRestore bool `json:"release_without_restore"`
}

// tenantQuotaLimit is the limit of a tenant's quota in one region.
type tenantQuotaLimit struct {
	CPU         int  `json:"cpu"`
	Memory      int  `json:"memory"`
	MemoryMax   int  `json:"memory_max"`
	Allocations *int `json:"allocations"`
	VariablesMB *int `json:"variables_mb"`
}

// tenantRole is a role of a tenant: the namespace capabilities of its
// people, their rights on the objects of kinds in the namespace (the kind
// as "<group>/<Kind>", to the object capabilities), and optionally a full
// ACL policy for what these can't say.
type tenantRole struct {
	Capabilities []string            `json:"capabilities"`
	Objects      map[string][]string `json:"objects"`
	Policy       string              `json:"policy"`
}

// tenantObjectKindRule is what the kind of a role's object rights may be:
// "<group>/<Kind>", either part with globs, or "*", as the ACL parser takes
// the kind of an object block. It is checked before the kind is written
// into the rules of a policy, so that the kind can't be anything but the
// key of its block.
var tenantObjectKindRule = regexp.MustCompile(`^[a-zA-Z0-9.*-]+/[a-zA-Z0-9*]+$|^\*$`)

// tenantResourceRef names a resource a tenant created. Pending marks one
// the tenant wrote down before it created it and has not seen to exist
// since: it may not exist (the creation fails, or has not happened yet).
type tenantResourceRef struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	ID        string `json:"id,omitempty"`
	Pending   bool   `json:"pending,omitempty"`

	// Creating is, for a pending resource, the fingerprint of what the
	// tenant is creating (tenantOriginal.fingerprint): how the tenant knows
	// its own creation when the resource exists and the status still says
	// pending.
	Creating string `json:"creating,omitempty"`

	// Adopted marks a resource that existed and the tenant took over
	// (spec.adopt), and Original is what it held then in the fields a
	// tenant writes. The tenant never deletes it: it gives it back as it
	// was (tenant_restore.go). A resource the tenant took over that was
	// deleted and that the tenant made again is the tenant's own: it loses
	// both.
	Adopted  bool            `json:"adopted,omitempty"`
	Original *tenantOriginal `json:"original,omitempty"`
}

func (r tenantResourceRef) key() string { return r.Type + "/" + r.Namespace + "/" + r.Name }

func (r tenantResourceRef) String() string {
	if r.Namespace != "" {
		return fmt.Sprintf("%s %s/%s", r.Type, r.Namespace, r.Name)
	}
	return r.Type + " " + r.Name
}

// tenantStatus is the status of a Tenant object.
type tenantStatus struct {
	Namespace string              `json:"namespace"`
	Resources []tenantResourceRef `json:"resources"`
}

// tenantDesired is what a tenant wants to exist.
type tenantDesired struct {
	namespace *structs.Namespace
	quota     *structs.QuotaSpec
	policies  []*structs.ACLPolicy
	roles     []*structs.ACLRole
	network   *structs.NetworkPolicy
}

// decodeTenantSpec decodes and checks the spec of a tenant.
func decodeTenantSpec(name string, spec map[string]any) (*tenantSpec, error) {
	out := &tenantSpec{}
	if err := structs.DecodeObjectPart(spec, out); err != nil {
		return nil, err
	}
	if out.Namespace.Name == "" {
		out.Namespace.Name = name
	}
	if out.Network.Isolation == "" {
		out.Network.Isolation = structs.TenantIsolationNone
	}
	return out, nil
}

// tenantNamespace returns the namespace of a tenant object.
func tenantNamespace(obj *structs.CustomObject) string {
	spec, err := decodeTenantSpec(obj.Name, obj.Spec)
	if err != nil {
		return ""
	}
	return spec.Namespace.Name
}

// roles returns the roles of the spec by name.
func (s *tenantSpec) roles() (map[string]*tenantRole, error) {
	out := map[string]*tenantRole{}
	for name, raw := range s.Roles {
		role := &tenantRole{}
		switch v := raw.(type) {
		case []any:
			for _, c := range v {
				str, ok := c.(string)
				if !ok {
					return nil, fmt.Errorf("role %q: a capability must be a string", name)
				}
				role.Capabilities = append(role.Capabilities, str)
			}
		case map[string]any:
			if err := structs.DecodeObjectPart(v, role); err != nil {
				return nil, fmt.Errorf("role %q: %v", name, err)
			}
			for k := range v {
				if k != "capabilities" && k != "objects" && k != "policy" {
					return nil, fmt.Errorf("role %q: unknown key %q (capabilities, objects, policy)", name, k)
				}
			}
			for kind := range role.Objects {
				if !tenantObjectKindRule.MatchString(kind) {
					return nil, fmt.Errorf("role %q: objects: the kind %q must be <group>/<Kind>", name, kind)
				}
			}
		default:
			return nil, fmt.Errorf("role %q: a list of capabilities or an object with capabilities, objects and policy", name)
		}
		out[name] = role
	}
	return out, nil
}

// policyRules returns the ACL policy of a role in the namespace.
func (r *tenantRole) policyRules(namespace string) string {
	var b strings.Builder
	quoted := func(list []string) string {
		out := make([]string, len(list))
		for i, c := range list {
			out[i] = fmt.Sprintf("%q", c)
		}
		return strings.Join(out, ", ")
	}
	if len(r.Capabilities) > 0 || len(r.Objects) > 0 {
		fmt.Fprintf(&b, "namespace %q {\n", namespace)
		if len(r.Capabilities) > 0 {
			fmt.Fprintf(&b, "  capabilities = [%s]\n", quoted(r.Capabilities))
		}
		kinds := make([]string, 0, len(r.Objects))
		for kind := range r.Objects {
			kinds = append(kinds, kind)
		}
		sort.Strings(kinds)
		for _, kind := range kinds {
			fmt.Fprintf(&b, "  object %q {\n    capabilities = [%s]\n  }\n", kind, quoted(r.Objects[kind]))
		}
		b.WriteString("}\n")
	}
	if r.Policy != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(strings.TrimSpace(r.Policy))
		b.WriteString("\n")
	}
	return b.String()
}

// tenantResourceName returns the name of the quota, policy or role of a
// tenant: the tenant's name, or "<tenant>-<role>".
func tenantResourceName(tenant, role string) string {
	if role == "" {
		return tenant
	}
	return tenant + "-" + role
}

// validate checks a decoded spec of the tenant of the name.
func (s *tenantSpec) validate(name string) error {
	if !tenantNameRule.MatchString(name) {
		return fmt.Errorf("the name of a tenant must match %s: the names of its resources derive from it", tenantNameRule)
	}
	ns := &structs.Namespace{Name: s.Namespace.Name, Description: s.Namespace.Description}
	if err := ns.Validate(); err != nil {
		return fmt.Errorf("namespace: %v", err)
	}
	if s.Namespace.Name == structs.DefaultNamespace {
		return fmt.Errorf("the namespace %q can't belong to a tenant", structs.DefaultNamespace)
	}
	for k := range s.Namespace.Meta {
		if strings.HasPrefix(k, "norca.io/certificate-") {
			return fmt.Errorf("namespace meta %q is written from certificate_dns_suffixes, certificate_ip_addresses and certificate_issuers", k)
		}
		if k == structs.NamespaceMetaDNSSuffixes {
			return fmt.Errorf("namespace meta %q is written from dns_suffixes", k)
		}
	}
	for _, suffix := range s.DNSSuffixes {
		if !validDNSSuffix(suffix) {
			return fmt.Errorf("dns_suffixes: %q is not a DNS name", suffix)
		}
	}
	if s.Quota != nil {
		if len(s.Quota.Region) == 0 {
			return fmt.Errorf("quota: at least one region block")
		}
	}
	roles, err := s.roles()
	if err != nil {
		return err
	}
	for roleName, role := range roles {
		if !tenantNameRule.MatchString(roleName) {
			return fmt.Errorf("role %q: the name must match %s", roleName, tenantNameRule)
		}
		if len(role.Capabilities) == 0 && len(role.Objects) == 0 && role.Policy == "" {
			return fmt.Errorf("role %q: no capabilities and no policy", roleName)
		}
		if _, err := acl.Parse(role.policyRules(s.Namespace.Name)); err != nil {
			return fmt.Errorf("role %q: %v", roleName, err)
		}
	}
	for _, other := range s.Network.AllowFromNamespaces {
		if other == "*" {
			return fmt.Errorf("network: allow_from_namespaces lists namespaces; with every namespace use isolation = %q", structs.TenantIsolationNone)
		}
	}
	if s.Network.Isolation != structs.TenantIsolationNamespace &&
		(len(s.Network.AllowFromCIDRs) > 0 || s.Network.Required != nil) {
		return fmt.Errorf("network: allow_from_cidrs and required need isolation = %q", structs.TenantIsolationNamespace)
	}
	for _, cidr := range s.Network.AllowFromCIDRs {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return fmt.Errorf("network: allow_from_cidrs: %q is not an IP prefix (address/length)", cidr)
		}
	}
	for _, addr := range s.CertificateIPAddresses {
		if net.ParseIP(addr) == nil {
			if _, _, err := net.ParseCIDR(addr); err != nil {
				return fmt.Errorf("certificate_ip_addresses: %q is not an IP address or prefix", addr)
			}
		}
	}
	for _, t := range s.Adopt {
		switch t {
		case structs.TenantResourceNamespace, structs.TenantResourceQuota, structs.TenantResourceACLPolicy,
			structs.TenantResourceACLRole, structs.TenantResourceNetworkPolicy:
		default:
			return fmt.Errorf("adopt: unknown resource type %q", t)
		}
	}
	return nil
}

// desired returns the resources the tenant of the name wants.
func (s *tenantSpec) desired(name string) (*tenantDesired, error) {
	d := &tenantDesired{}
	ns := s.Namespace.Name

	meta := map[string]string{}
	for k, v := range s.Namespace.Meta {
		meta[k] = v
	}
	if len(s.CertificateDNSSuffixes) > 0 {
		meta[structs.NamespaceMetaCertificateDNSSuffixes] = strings.Join(s.CertificateDNSSuffixes, ",")
	}
	if len(s.CertificateIPAddresses) > 0 {
		meta[structs.NamespaceMetaCertificateIPAddresses] = strings.Join(s.CertificateIPAddresses, ",")
	}
	if len(s.CertificateIssuers) > 0 {
		meta[structs.NamespaceMetaCertificateIssuers] = strings.Join(s.CertificateIssuers, ",")
	}
	if len(s.DNSSuffixes) > 0 {
		meta[structs.NamespaceMetaDNSSuffixes] = strings.Join(s.DNSSuffixes, ",")
	}
	if len(meta) == 0 {
		meta = nil
	}
	d.namespace = &structs.Namespace{Name: ns, Description: s.Namespace.Description, Meta: meta}

	if s.Quota != nil {
		q := &structs.QuotaSpec{
			Name:        tenantResourceName(name, ""),
			Description: s.Quota.Description,
			Enforce:     s.Quota.Enforce,
		}
		if q.Description == "" {
			q.Description = "Quota of the tenant " + name
		}
		regions := make([]string, 0, len(s.Quota.Region))
		for r := range s.Quota.Region {
			regions = append(regions, r)
		}
		sort.Strings(regions)
		for _, r := range regions {
			l := s.Quota.Region[r]
			if l == nil {
				l = &tenantQuotaLimit{}
			}
			limit := &structs.QuotaLimit{
				Region:      r,
				RegionLimit: &structs.Resources{CPU: l.CPU, MemoryMB: l.Memory, MemoryMaxMB: l.MemoryMax},
				Allocations: l.Allocations,
			}
			if l.VariablesMB != nil {
				v := *l.VariablesMB
				limit.VariablesLimit = &v
			}
			q.Limits = append(q.Limits, limit)
		}
		d.quota = q
		d.namespace.Quota = q.Name
	}

	roles, err := s.roles()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(roles))
	for r := range roles {
		names = append(names, r)
	}
	sort.Strings(names)
	for _, r := range names {
		policy := &structs.ACLPolicy{
			Name:        tenantResourceName(name, r),
			Description: fmt.Sprintf("Role %s of the tenant %s", r, name),
			Rules:       roles[r].policyRules(ns),
		}
		d.policies = append(d.policies, policy)
		d.roles = append(d.roles, &structs.ACLRole{
			Name:        policy.Name,
			Description: policy.Description,
			Policies:    []*structs.ACLRolePolicyLink{{Name: policy.Name}},
		})
	}

	if s.Network.Isolation == structs.TenantIsolationNamespace {
		peers := []*structs.NetworkPolicyPeer{{}}
		others := slices.Clone(s.Network.AllowFromNamespaces)
		sort.Strings(others)
		for _, other := range slices.Compact(others) {
			if other != ns {
				peers = append(peers, &structs.NetworkPolicyPeer{Namespace: other})
			}
		}
		if len(s.Network.AllowFromCIDRs) > 0 {
			cidrs := slices.Clone(s.Network.AllowFromCIDRs)
			sort.Strings(cidrs)
			peers = append(peers, &structs.NetworkPolicyPeer{CIDR: slices.Compact(cidrs)})
		}
		enforcement := structs.NetworkPolicyEnforcementRequired
		if s.Network.Required != nil && !*s.Network.Required {
			enforcement = structs.NetworkPolicyEnforcementBestEffort
		}
		d.network = &structs.NetworkPolicy{
			Name:        structs.TenantNetworkPolicyName,
			Namespace:   ns,
			Description: fmt.Sprintf("Isolation of the tenant %s: traffic in from its own namespace and the listed ones only", name),
			Enforcement: enforcement,
			Ingress:     []*structs.NetworkPolicyRule{{Name: "tenant", Peers: peers}},
		}
	}
	return d, nil
}

// validDNSSuffix reports whether a suffix of dns_suffixes is a DNS name
// (lower or upper case letters, digits and hyphens in labels of 1 to 63,
// optionally "*." in front, which allows the same names).
func validDNSSuffix(s string) bool {
	s = strings.TrimSuffix(strings.TrimPrefix(s, "*."), ".")
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

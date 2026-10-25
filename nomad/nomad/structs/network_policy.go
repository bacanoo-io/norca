// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"fmt"
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/go-multierror"
)

// Network rules (Kubernetes features spec, item 7, levels A and B).
//
// A NetworkPolicy belongs to a namespace and selects the allocations of the
// namespace by the merged meta of their job and task group. Being selected
// means default deny in each direction the policy has rules for; the rules
// of all policies selecting an allocation add up, and the filtering is
// stateful, so replies to allowed connections need no rule.
//
// An AdminNetworkPolicy is a cluster object evaluated before the namespace
// policies. Its rules are ordered by the policy priority and have an
// explicit action: "allow" and "deny" are final and cannot be overridden by
// namespace policies, "pass" hands the decision over to them.

const (
	// NetworkPolicyEnforcementRequired rejects placements on nodes that
	// cannot enforce the rules of the policy.
	NetworkPolicyEnforcementRequired = "required"

	// NetworkPolicyEnforcementBestEffort places allocations on nodes that
	// cannot enforce the rules, with a warning.
	NetworkPolicyEnforcementBestEffort = "best_effort"

	// Actions of AdminNetworkPolicy rules.
	NetworkPolicyActionAllow = "allow"
	NetworkPolicyActionDeny  = "deny"
	NetworkPolicyActionPass  = "pass"

	// Directions of the rules.
	NetworkPolicyDirectionIngress = "ingress"
	NetworkPolicyDirectionEgress  = "egress"

	// NetworkPolicyNftablesAttr is the node attribute set by the client
	// fingerprint when the node can enforce network rules with nftables.
	NetworkPolicyNftablesAttr = "norca.network_policy.nftables"

	// TopicNetworkPolicy is the event stream topic of network policy and
	// admin network policy changes.
	TopicNetworkPolicy Topic = "NetworkPolicy"

	TypeNetworkPolicyUpserted      = "NetworkPolicyUpserted"
	TypeNetworkPolicyDeleted       = "NetworkPolicyDeleted"
	TypeAdminNetworkPolicyUpserted = "AdminNetworkPolicyUpserted"
	TypeAdminNetworkPolicyDeleted  = "AdminNetworkPolicyDeleted"

	// AdminNetworkPolicyMaxPriority is the largest allowed priority. Lower
	// priorities are evaluated first.
	AdminNetworkPolicyMaxPriority = 1000

	maxNetworkPolicyDescriptionLength = 1024
	maxNetworkPolicyRules             = 64
	maxNetworkPolicyPeers             = 64
	maxNetworkPolicyCIDRs             = 256
	maxNetworkPolicyPorts             = 64
	maxNetworkPolicySelectorKeys      = 32
)

var (
	// validNetworkPolicyName is the rule used to validate policy and rule
	// names.
	validNetworkPolicyName = regexp.MustCompile("^[a-zA-Z0-9-_]{1,128}$")

	// validNetworkPolicyPortLabel is the rule used to validate port labels
	// in port specifications.
	validNetworkPolicyPortLabel = regexp.MustCompile("^[a-zA-Z_][a-zA-Z0-9_]*$")
)

// NetworkPolicy restricts the traffic of the allocations of a namespace.
type NetworkPolicy struct {
	Name        string
	Namespace   string
	Description string

	// Selector selects the allocations whose merged job and task group meta
	// contains every key with the given value. An empty selector selects
	// every allocation of the namespace.
	Selector map[string]string

	// Ingress and Egress are the allowed traffic. A direction without rules
	// is not restricted by the policy.
	Ingress []*NetworkPolicyRule
	Egress  []*NetworkPolicyRule

	// DefaultDeny lists directions ("ingress", "egress") the policy
	// isolates even without rules, e.g. to deny all ingress traffic.
	DefaultDeny []string

	// Enforcement is "required" (default) or "best_effort".
	Enforcement string

	CreateIndex uint64
	ModifyIndex uint64
}

// AdminNetworkPolicy is a cluster level network policy with priority and
// explicit actions that namespace policies cannot override.
type AdminNetworkPolicy struct {
	Name        string
	Description string

	// Priority orders the admin policies; lower values are evaluated first.
	// Policies with the same priority are ordered by name.
	Priority int

	// Namespaces restricts the policy to namespaces (exact names or glob
	// patterns). Empty means every namespace.
	Namespaces []string

	// Selector selects allocations by their merged job and task group meta.
	// Empty selects every allocation of the matching namespaces.
	Selector map[string]string

	Ingress []*NetworkPolicyRule
	Egress  []*NetworkPolicyRule

	// Enforcement is "required" (default) or "best_effort".
	Enforcement string

	CreateIndex uint64
	ModifyIndex uint64
}

// NetworkPolicyRule is an ingress or egress rule. Traffic matches the rule
// if it matches one of the peers (any peer if there are none) and one of the
// ports (any port if there are none).
type NetworkPolicyRule struct {
	// Name optionally identifies the rule in the drop counter metrics.
	Name string

	// Action is only used by admin policies: "allow", "deny" or "pass".
	Action string

	// Peers are the sources (ingress, "from" blocks) or destinations
	// (egress, "to" blocks) of the traffic.
	Peers []*NetworkPolicyPeer

	// Ports are port specifications: "5432/tcp", "8000-8100/udp", "53"
	// (TCP), or a port label of the allocation, e.g. "db/tcp" (ingress
	// only). Ingress ports are the ports inside the allocation network
	// namespace, i.e. the "to" side of a port mapping.
	Ports []string
}

// NetworkPolicyPeer is a peer of a rule. It is either an address block
// (CIDR) or a set of allocations running on the same node, selected by
// namespace and meta.
type NetworkPolicyPeer struct {
	// CIDR lists IPv4 or IPv6 prefixes.
	CIDR []string

	// Namespace selects the namespace of peer allocations: empty means the
	// namespace of the policy (every namespace for admin policies), "*"
	// every namespace. Must be empty for CIDR peers.
	Namespace string

	// Selector selects peer allocations by meta. Empty selects every
	// allocation of the namespace. Must be empty for CIDR peers.
	Selector map[string]string
}

// NetworkPolicyPort is a parsed port specification.
type NetworkPolicyPort struct {
	Protocol string
	From     uint16
	To       uint16

	// Label is set instead of From and To for port labels.
	Label string
}

// ParseNetworkPolicyPort parses a port specification of a rule.
func ParseNetworkPolicyPort(spec string) (*NetworkPolicyPort, error) {
	port, proto, found := strings.Cut(strings.TrimSpace(spec), "/")
	if !found {
		proto = "tcp"
	}
	proto = strings.ToLower(proto)
	switch proto {
	case "tcp", "udp", "sctp":
	default:
		return nil, fmt.Errorf("port %q: invalid protocol %q, must be tcp, udp or sctp", spec, proto)
	}
	if port == "" {
		return nil, fmt.Errorf("port %q: missing port", spec)
	}

	out := &NetworkPolicyPort{Protocol: proto}
	if validNetworkPolicyPortLabel.MatchString(port) {
		out.Label = port
		return out, nil
	}

	from, to, isRange := strings.Cut(port, "-")
	lo, err := strconv.ParseUint(from, 10, 16)
	if err != nil || lo == 0 {
		return nil, fmt.Errorf("port %q: invalid port %q", spec, from)
	}
	hi := lo
	if isRange {
		hi, err = strconv.ParseUint(to, 10, 16)
		if err != nil || hi == 0 {
			return nil, fmt.Errorf("port %q: invalid port %q", spec, to)
		}
		if hi < lo {
			return nil, fmt.Errorf("port %q: range end is lower than its start", spec)
		}
	}
	out.From, out.To = uint16(lo), uint16(hi)
	return out, nil
}

// String returns the canonical specification of the port.
func (p *NetworkPolicyPort) String() string {
	switch {
	case p.Label != "":
		return p.Label + "/" + p.Protocol
	case p.From == p.To:
		return fmt.Sprintf("%d/%s", p.From, p.Protocol)
	default:
		return fmt.Sprintf("%d-%d/%s", p.From, p.To, p.Protocol)
	}
}

// GetID returns the name of the policy (paginator support).
func (p *NetworkPolicy) GetID() string { return p.Name }

// GetNamespace returns the namespace of the policy (paginator support).
func (p *NetworkPolicy) GetNamespace() string { return p.Namespace }

// GetCreateIndex returns the create index of the policy.
func (p *NetworkPolicy) GetCreateIndex() uint64 { return p.CreateIndex }

// Canonicalize sets the defaults of the policy.
func (p *NetworkPolicy) Canonicalize() {
	if p.Namespace == "" {
		p.Namespace = DefaultNamespace
	}
	if p.Enforcement == "" {
		p.Enforcement = NetworkPolicyEnforcementRequired
	}
}

// Validate returns an error if the policy is invalid.
func (p *NetworkPolicy) Validate() error {
	var mErr *multierror.Error
	if !validNetworkPolicyName.MatchString(p.Name) {
		mErr = multierror.Append(mErr, fmt.Errorf("invalid name %q, must match regex %s", p.Name, validNetworkPolicyName))
	}
	if !validNamespaceName.MatchString(p.Namespace) {
		mErr = multierror.Append(mErr, fmt.Errorf("invalid namespace %q", p.Namespace))
	}
	mErr = validateNetworkPolicyCommon(mErr, p.Description, p.Selector, p.Enforcement)
	for _, dir := range p.DefaultDeny {
		switch dir {
		case NetworkPolicyDirectionIngress, NetworkPolicyDirectionEgress:
		default:
			mErr = multierror.Append(mErr, fmt.Errorf("invalid default_deny direction %q, must be %q or %q",
				dir, NetworkPolicyDirectionIngress, NetworkPolicyDirectionEgress))
		}
	}
	if len(p.Ingress) == 0 && len(p.Egress) == 0 && len(p.DefaultDeny) == 0 {
		// A policy without rules and without default_deny would not
		// restrict anything.
		mErr = multierror.Append(mErr, fmt.Errorf("at least one ingress or egress rule or a default_deny direction is required"))
	}
	mErr = validateNetworkPolicyRules(mErr, NetworkPolicyDirectionIngress, p.Ingress, false)
	mErr = validateNetworkPolicyRules(mErr, NetworkPolicyDirectionEgress, p.Egress, false)
	return mErr.ErrorOrNil()
}

// Copy returns a deep copy of the policy.
func (p *NetworkPolicy) Copy() *NetworkPolicy {
	if p == nil {
		return nil
	}
	np := new(NetworkPolicy)
	*np = *p
	np.Selector = maps.Clone(p.Selector)
	np.Ingress = copyNetworkPolicyRules(p.Ingress)
	np.Egress = copyNetworkPolicyRules(p.Egress)
	np.DefaultDeny = slices.Clone(p.DefaultDeny)
	return np
}

// Isolates reports whether the policy isolates the direction: selected
// allocations only get the traffic allowed by rules in that direction.
func (p *NetworkPolicy) Isolates(direction string) bool {
	return len(p.Rules(direction)) > 0 || slices.Contains(p.DefaultDeny, direction)
}

// Rules returns the rules of the direction.
func (p *NetworkPolicy) Rules(direction string) []*NetworkPolicyRule {
	if direction == NetworkPolicyDirectionIngress {
		return p.Ingress
	}
	return p.Egress
}

// Selects reports whether the policy selects allocations of a task group in
// the namespace with the given merged meta.
func (p *NetworkPolicy) Selects(namespace string, meta map[string]string) bool {
	return p.Namespace == namespace && NetworkPolicySelectorMatches(p.Selector, meta)
}

// Required reports whether the policy requires enforcement.
func (p *NetworkPolicy) Required() bool {
	return p.Enforcement != NetworkPolicyEnforcementBestEffort
}

// GetID returns the name of the policy (paginator support).
func (p *AdminNetworkPolicy) GetID() string { return p.Name }

// GetCreateIndex returns the create index of the policy.
func (p *AdminNetworkPolicy) GetCreateIndex() uint64 { return p.CreateIndex }

// Canonicalize sets the defaults of the policy.
func (p *AdminNetworkPolicy) Canonicalize() {
	if p.Enforcement == "" {
		p.Enforcement = NetworkPolicyEnforcementRequired
	}
}

// Validate returns an error if the policy is invalid.
func (p *AdminNetworkPolicy) Validate() error {
	var mErr *multierror.Error
	if !validNetworkPolicyName.MatchString(p.Name) {
		mErr = multierror.Append(mErr, fmt.Errorf("invalid name %q, must match regex %s", p.Name, validNetworkPolicyName))
	}
	if p.Priority < 0 || p.Priority > AdminNetworkPolicyMaxPriority {
		mErr = multierror.Append(mErr, fmt.Errorf("priority must be between 0 and %d", AdminNetworkPolicyMaxPriority))
	}
	for _, ns := range p.Namespaces {
		if ns == "" {
			mErr = multierror.Append(mErr, fmt.Errorf("empty namespace pattern"))
		}
	}
	mErr = validateNetworkPolicyCommon(mErr, p.Description, p.Selector, p.Enforcement)
	if len(p.Ingress) == 0 && len(p.Egress) == 0 {
		mErr = multierror.Append(mErr, fmt.Errorf("at least one ingress or egress rule is required"))
	}
	mErr = validateNetworkPolicyRules(mErr, NetworkPolicyDirectionIngress, p.Ingress, true)
	mErr = validateNetworkPolicyRules(mErr, NetworkPolicyDirectionEgress, p.Egress, true)
	return mErr.ErrorOrNil()
}

// Copy returns a deep copy of the policy.
func (p *AdminNetworkPolicy) Copy() *AdminNetworkPolicy {
	if p == nil {
		return nil
	}
	np := new(AdminNetworkPolicy)
	*np = *p
	np.Namespaces = slices.Clone(p.Namespaces)
	np.Selector = maps.Clone(p.Selector)
	np.Ingress = copyNetworkPolicyRules(p.Ingress)
	np.Egress = copyNetworkPolicyRules(p.Egress)
	return np
}

// Rules returns the rules of the direction.
func (p *AdminNetworkPolicy) Rules(direction string) []*NetworkPolicyRule {
	if direction == NetworkPolicyDirectionIngress {
		return p.Ingress
	}
	return p.Egress
}

// Selects reports whether the policy selects allocations of a task group in
// the namespace with the given merged meta.
func (p *AdminNetworkPolicy) Selects(namespace string, meta map[string]string) bool {
	return AdmissionNamespaceMatches(p.Namespaces, namespace, true) &&
		NetworkPolicySelectorMatches(p.Selector, meta)
}

// Required reports whether the policy requires enforcement.
func (p *AdminNetworkPolicy) Required() bool {
	return p.Enforcement != NetworkPolicyEnforcementBestEffort
}

// SortAdminNetworkPolicies sorts admin policies in evaluation order: by
// priority, then by name.
func SortAdminNetworkPolicies(policies []*AdminNetworkPolicy) {
	sort.SliceStable(policies, func(i, j int) bool {
		if policies[i].Priority != policies[j].Priority {
			return policies[i].Priority < policies[j].Priority
		}
		return policies[i].Name < policies[j].Name
	})
}

// Matches reports whether the peer is an allocation peer selecting an
// allocation of the namespace with the given meta. policyNamespace is the
// namespace of the policy; it is empty for admin policies.
func (p *NetworkPolicyPeer) Matches(policyNamespace, namespace string, meta map[string]string) bool {
	if p == nil || len(p.CIDR) > 0 {
		return false
	}
	switch {
	case p.Namespace == "*":
	case p.Namespace != "":
		if p.Namespace != namespace {
			return false
		}
	case policyNamespace != "":
		if policyNamespace != namespace {
			return false
		}
	}
	return NetworkPolicySelectorMatches(p.Selector, meta)
}

// NetworkPolicySelectorMatches reports whether every key of the selector is
// in meta with the same value. An empty selector matches everything.
func NetworkPolicySelectorMatches(selector, meta map[string]string) bool {
	for k, v := range selector {
		if mv, ok := meta[k]; !ok || mv != v {
			return false
		}
	}
	return true
}

// NetworkPolicyMeta returns the meta that network policy selectors match:
// the job meta overridden by the task group meta.
func NetworkPolicyMeta(job *Job, tg *TaskGroup) map[string]string {
	meta := make(map[string]string)
	if job != nil {
		maps.Copy(meta, job.Meta)
	}
	if tg != nil {
		maps.Copy(meta, tg.Meta)
	}
	return meta
}

// NetworkPolicyGroupMode returns the network mode of the task group as used
// by network policy enforcement ("host" when the group has no network).
func NetworkPolicyGroupMode(tg *TaskGroup) string {
	if tg == nil || len(tg.Networks) == 0 || tg.Networks[0].Mode == "" {
		return "host"
	}
	return tg.Networks[0].Mode
}

// NetworkPolicyModeEnforceable reports whether network rules can be enforced
// for a task group network mode. Rules are written into the network
// namespace of the allocation, so only bridge and CNI modes can enforce
// them; "none" has no traffic to filter.
func NetworkPolicyModeEnforceable(mode string) bool {
	return mode == "bridge" || mode == "none" || strings.HasPrefix(mode, "cni/")
}

// NetworkPolicyModeNeedsNftables reports whether enforcing the rules in the
// network mode needs nftables on the node.
func NetworkPolicyModeNeedsNftables(mode string) bool {
	return mode != "none"
}

// NetworkPolicyNodeCanEnforce reports whether network rules can be enforced
// for an allocation in the network mode on the node, and the reason if not.
func NetworkPolicyNodeCanEnforce(node *Node, mode string) (bool, string) {
	if !NetworkPolicyModeEnforceable(mode) {
		return false, fmt.Sprintf("network mode %q cannot enforce network policies", mode)
	}
	if !NetworkPolicyModeNeedsNftables(mode) {
		return true, ""
	}
	if node == nil || node.Attributes[NetworkPolicyNftablesAttr] != "true" {
		return false, "node does not support nftables network policy enforcement"
	}
	return true, ""
}

func validateNetworkPolicyCommon(mErr *multierror.Error, description string, selector map[string]string, enforcement string) *multierror.Error {
	if len(description) > maxNetworkPolicyDescriptionLength {
		mErr = multierror.Append(mErr, fmt.Errorf("description longer than %d", maxNetworkPolicyDescriptionLength))
	}
	if len(selector) > maxNetworkPolicySelectorKeys {
		mErr = multierror.Append(mErr, fmt.Errorf("selector has more than %d keys", maxNetworkPolicySelectorKeys))
	}
	for k := range selector {
		if k == "" {
			mErr = multierror.Append(mErr, fmt.Errorf("selector has an empty key"))
		}
	}
	switch enforcement {
	case NetworkPolicyEnforcementRequired, NetworkPolicyEnforcementBestEffort:
	default:
		mErr = multierror.Append(mErr, fmt.Errorf("invalid enforcement %q, must be %q or %q",
			enforcement, NetworkPolicyEnforcementRequired, NetworkPolicyEnforcementBestEffort))
	}
	return mErr
}

func validateNetworkPolicyRules(mErr *multierror.Error, direction string, rules []*NetworkPolicyRule, admin bool) *multierror.Error {
	if len(rules) > maxNetworkPolicyRules {
		mErr = multierror.Append(mErr, fmt.Errorf("%s: at most %d rules are allowed", direction, maxNetworkPolicyRules))
	}
	names := make(map[string]struct{})
	for i, rule := range rules {
		prefix := fmt.Sprintf("%s rule %d", direction, i)
		if rule == nil {
			mErr = multierror.Append(mErr, fmt.Errorf("%s: empty rule", prefix))
			continue
		}
		if rule.Name != "" {
			if !validNetworkPolicyName.MatchString(rule.Name) {
				mErr = multierror.Append(mErr, fmt.Errorf("%s: invalid name %q", prefix, rule.Name))
			}
			if _, ok := names[rule.Name]; ok {
				mErr = multierror.Append(mErr, fmt.Errorf("%s: duplicate rule name %q", prefix, rule.Name))
			}
			names[rule.Name] = struct{}{}
		}
		switch {
		case admin:
			switch rule.Action {
			case NetworkPolicyActionAllow, NetworkPolicyActionDeny, NetworkPolicyActionPass:
			default:
				mErr = multierror.Append(mErr, fmt.Errorf("%s: invalid action %q, must be %q, %q or %q", prefix,
					rule.Action, NetworkPolicyActionAllow, NetworkPolicyActionDeny, NetworkPolicyActionPass))
			}
		case rule.Action != "" && rule.Action != NetworkPolicyActionAllow:
			mErr = multierror.Append(mErr, fmt.Errorf("%s: namespace policies only allow traffic, action %q is reserved for admin network policies", prefix, rule.Action))
		}
		if len(rule.Peers) > maxNetworkPolicyPeers {
			mErr = multierror.Append(mErr, fmt.Errorf("%s: at most %d peers are allowed", prefix, maxNetworkPolicyPeers))
		}
		for j, peer := range rule.Peers {
			if peer == nil {
				mErr = multierror.Append(mErr, fmt.Errorf("%s peer %d: empty peer", prefix, j))
				continue
			}
			if len(peer.CIDR) > 0 && (peer.Namespace != "" || len(peer.Selector) > 0) {
				mErr = multierror.Append(mErr, fmt.Errorf("%s peer %d: cidr cannot be combined with namespace or selector", prefix, j))
			}
			if len(peer.CIDR) > maxNetworkPolicyCIDRs {
				mErr = multierror.Append(mErr, fmt.Errorf("%s peer %d: at most %d CIDRs are allowed", prefix, j, maxNetworkPolicyCIDRs))
			}
			for _, cidr := range peer.CIDR {
				if _, err := netip.ParsePrefix(cidr); err != nil {
					mErr = multierror.Append(mErr, fmt.Errorf("%s peer %d: invalid CIDR %q", prefix, j, cidr))
				}
			}
			if peer.Namespace != "" && peer.Namespace != "*" && !validNamespaceName.MatchString(peer.Namespace) {
				mErr = multierror.Append(mErr, fmt.Errorf("%s peer %d: invalid namespace %q", prefix, j, peer.Namespace))
			}
			if len(peer.Selector) > maxNetworkPolicySelectorKeys {
				mErr = multierror.Append(mErr, fmt.Errorf("%s peer %d: selector has more than %d keys", prefix, j, maxNetworkPolicySelectorKeys))
			}
		}
		if len(rule.Ports) > maxNetworkPolicyPorts {
			mErr = multierror.Append(mErr, fmt.Errorf("%s: at most %d ports are allowed", prefix, maxNetworkPolicyPorts))
		}
		for _, spec := range rule.Ports {
			port, err := ParseNetworkPolicyPort(spec)
			if err != nil {
				mErr = multierror.Append(mErr, fmt.Errorf("%s: %v", prefix, err))
				continue
			}
			if port.Label != "" && direction == NetworkPolicyDirectionEgress {
				mErr = multierror.Append(mErr, fmt.Errorf("%s: port labels are only allowed in ingress rules", prefix))
			}
		}
	}
	return mErr
}

func copyNetworkPolicyRules(rules []*NetworkPolicyRule) []*NetworkPolicyRule {
	if rules == nil {
		return nil
	}
	out := make([]*NetworkPolicyRule, len(rules))
	for i, r := range rules {
		if r == nil {
			continue
		}
		nr := *r
		nr.Ports = slices.Clone(r.Ports)
		if r.Peers != nil {
			nr.Peers = make([]*NetworkPolicyPeer, len(r.Peers))
			for j, p := range r.Peers {
				if p == nil {
					continue
				}
				nr.Peers[j] = &NetworkPolicyPeer{
					CIDR:      slices.Clone(p.CIDR),
					Namespace: p.Namespace,
					Selector:  maps.Clone(p.Selector),
				}
			}
		}
		out[i] = &nr
	}
	return out
}

// NetworkPolicyListRequest is used to list network policies. The namespace
// of the query options selects the namespace ("*" for all).
type NetworkPolicyListRequest struct {
	QueryOptions
}

// NetworkPolicyListResponse is the response to a network policy list request.
type NetworkPolicyListResponse struct {
	NetworkPolicies []*NetworkPolicy
	QueryMeta
}

// NetworkPolicySpecificRequest is used to query a network policy of the
// namespace of the query options.
type NetworkPolicySpecificRequest struct {
	Name string
	QueryOptions
}

// SingleNetworkPolicyResponse is the response to a network policy query.
type SingleNetworkPolicyResponse struct {
	NetworkPolicy *NetworkPolicy
	QueryMeta
}

// NetworkPolicyUpsertRequest is used to create or update network policies.
type NetworkPolicyUpsertRequest struct {
	NetworkPolicies []*NetworkPolicy
	WriteRequest
}

// NetworkPolicyDeleteRequest is used to delete network policies of the
// namespace of the write request.
type NetworkPolicyDeleteRequest struct {
	Names []string
	WriteRequest
}

// AdminNetworkPolicyListRequest is used to list admin network policies.
type AdminNetworkPolicyListRequest struct {
	QueryOptions
}

// AdminNetworkPolicyListResponse is the response to an admin network policy
// list request.
type AdminNetworkPolicyListResponse struct {
	AdminNetworkPolicies []*AdminNetworkPolicy
	QueryMeta
}

// AdminNetworkPolicySpecificRequest is used to query an admin network policy.
type AdminNetworkPolicySpecificRequest struct {
	Name string
	QueryOptions
}

// SingleAdminNetworkPolicyResponse is the response to an admin network
// policy query.
type SingleAdminNetworkPolicyResponse struct {
	AdminNetworkPolicy *AdminNetworkPolicy
	QueryMeta
}

// AdminNetworkPolicyUpsertRequest is used to create or update admin network
// policies.
type AdminNetworkPolicyUpsertRequest struct {
	AdminNetworkPolicies []*AdminNetworkPolicy
	WriteRequest
}

// AdminNetworkPolicyDeleteRequest is used to delete admin network policies.
type AdminNetworkPolicyDeleteRequest struct {
	Names []string
	WriteRequest
}

// NetworkPolicyStatusRequest is used to list the allocations a network
// policy (or, with Admin set, an admin network policy) selects.
type NetworkPolicyStatusRequest struct {
	Name  string
	Admin bool
	QueryOptions
}

// NetworkPolicyStatusResponse is the response to a status request.
type NetworkPolicyStatusResponse struct {
	Allocations []*NetworkPolicyAllocStatus
	QueryMeta
}

// NetworkPolicyAllocStatus describes a non-terminal allocation selected by a
// policy and whether its node can enforce the rules.
type NetworkPolicyAllocStatus struct {
	ID          string
	Namespace   string
	JobID       string
	TaskGroup   string
	NodeID      string
	NodeName    string
	NetworkMode string
	Enforceable bool
	Reason      string
}

// NetworkPolicyAllocRequest is used by clients to fetch the network rules of
// one of their allocations. It supports blocking queries.
type NetworkPolicyAllocRequest struct {
	AllocID string
	QueryOptions
}

// NetworkPolicyAllocResponse is the response to a NetworkPolicyAllocRequest.
type NetworkPolicyAllocResponse struct {
	Rules *NetworkPolicyAllocRules
	QueryMeta
}

// NetworkPolicyAllocRules is the effective rule set of an allocation: the
// policies selecting it and the other allocations of its node that peers of
// these policies select.
type NetworkPolicyAllocRules struct {
	// Policies are the namespace policies selecting the allocation, sorted
	// by name.
	Policies []*NetworkPolicy

	// AdminPolicies are the admin policies selecting the allocation, in
	// evaluation order.
	AdminPolicies []*AdminNetworkPolicy

	// Peers are the other running allocations of the node with an address
	// that at least one allocation peer of the policies selects.
	Peers []*NetworkPolicyPeerAlloc
}

// Empty reports whether no policy selects the allocation.
func (r *NetworkPolicyAllocRules) Empty() bool {
	return r == nil || (len(r.Policies) == 0 && len(r.AdminPolicies) == 0)
}

// NetworkPolicyPeerAlloc is an allocation that can be the peer of a rule.
type NetworkPolicyPeerAlloc struct {
	ID        string
	Namespace string
	Meta      map[string]string
	Address   string
}

// NetworkPolicyEvent is the event stream payload of network policy changes.
type NetworkPolicyEvent struct {
	NetworkPolicy *NetworkPolicy
}

// AdminNetworkPolicyEvent is the event stream payload of admin network
// policy changes.
type AdminNetworkPolicyEvent struct {
	AdminNetworkPolicy *AdminNetworkPolicy
}

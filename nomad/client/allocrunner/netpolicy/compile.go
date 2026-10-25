// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package netpolicy compiles the network policies selecting an allocation
// into an nftables rule set description and applies it in the network
// namespace of the allocation.
//
// Compile is a pure function, so the semantics of network policies can be
// tested without root; only Apply and ReadCounters (Linux) touch the
// kernel.
//
// The rule set lives in the table "inet norca" of the allocation network
// namespace and has one base chain per restricted direction (hook input for
// ingress, hook output for egress):
//
//	chain ingress {
//	  type filter hook input priority 0; policy accept;
//	  iifname "lo" accept                      # loopback
//	  ct state established,related accept     # stateful filtering
//	  ct state invalid drop
//	  icmpv6 type { nd-* } accept              # IPv6 neighbor discovery
//	  ip saddr <node> accept                   # the node itself (ingress)
//	  jump ingress_admin                       # admin layer, if any
//	  jump ingress_ns                          # namespace layer, if isolated
//	}
//
// The admin chain holds the admin rules in priority order: allow accepts,
// deny counts and drops, pass returns to the base chain, which then
// evaluates the namespace layer. The namespace chain accepts the traffic the
// rules of the namespace policies allow and counts and drops the rest.
package netpolicy

import (
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// TableName is the name of the inet table holding the rule set.
	TableName = "norca"

	// Protocol numbers.
	ProtoTCP    uint8 = 6
	ProtoUDP    uint8 = 17
	ProtoSCTP   uint8 = 132
	ProtoICMPv6 uint8 = 58
)

// Verdict is the verdict of a rule.
type Verdict int

const (
	VerdictAccept Verdict = iota
	VerdictDrop
	// VerdictReturn ends the admin layer ("pass").
	VerdictReturn
)

func (v Verdict) String() string {
	switch v {
	case VerdictAccept:
		return "accept"
	case VerdictDrop:
		return "drop"
	default:
		return "return"
	}
}

// Input is the input of Compile.
type Input struct {
	// Namespace of the allocation.
	Namespace string

	// Rules is the effective rule set from the servers.
	Rules *structs.NetworkPolicyAllocRules

	// Ports are the allocated ports of the allocation, used to resolve the
	// port labels of ingress rules.
	Ports structs.AllocatedPorts

	// NodeAddrs are the addresses the node itself uses towards the
	// allocation (e.g. the bridge gateway). Ingress from them is always
	// allowed so the client can run health checks.
	NodeAddrs []netip.Addr
}

// Ruleset is the compiled rule set of an allocation. A nil Ruleset means no
// restriction: the table must not exist.
type Ruleset struct {
	// Ingress and Egress are nil for unrestricted directions.
	Ingress *Chain
	Egress  *Chain

	// Counters are the drop counters of the rule set.
	Counters []*Counter
}

// Chain is the compiled rules of a direction.
type Chain struct {
	Direction string

	// NodeAddrs are always allowed (ingress only).
	NodeAddrs []netip.Addr

	// Admin is the admin layer in evaluation order.
	Admin []*Rule

	// Isolated is set when namespace policies isolate the direction: only
	// the Allow rules are accepted, the rest is dropped and counted by
	// DefaultCounter.
	Isolated       bool
	Allow          []*Rule
	DefaultCounter string
}

// Rule is a compiled rule. It matches traffic whose peer address is in one
// of the prefixes (any address if Prefixes is nil) and whose destination
// port is in one of the port ranges (any port and protocol if Ports is
// nil).
type Rule struct {
	Verdict  Verdict
	Counter  string
	Prefixes []netip.Prefix
	Ports    []PortRange

	// Origin describes the policy and rule the rule comes from.
	Origin string
}

// PortRange is a destination port range of a protocol.
type PortRange struct {
	Proto uint8
	From  uint16
	To    uint16
}

// Counter is a named drop counter.
type Counter struct {
	Name      string
	Direction string

	// Kind is "admin" for admin deny rules and "default-deny" for the
	// default deny of the namespace layer.
	Kind string

	// Policy is the admin policy name, or the names of the namespace
	// policies isolating the direction (comma separated).
	Policy string

	// Rule is the rule name (or index) of admin deny rules.
	Rule string
}

// Empty reports whether the rule set has no restriction.
func (r *Ruleset) Empty() bool {
	return r == nil || (r.Ingress == nil && r.Egress == nil)
}

// Compile compiles the effective rules of an allocation. It returns nil if
// no policy restricts the traffic of the allocation.
func Compile(in *Input) (*Ruleset, error) {
	if in == nil || in.Rules.Empty() {
		return nil, nil
	}
	rs := &Ruleset{}
	var err error
	if rs.Ingress, err = compileDirection(in, rs, structs.NetworkPolicyDirectionIngress); err != nil {
		return nil, err
	}
	if rs.Egress, err = compileDirection(in, rs, structs.NetworkPolicyDirectionEgress); err != nil {
		return nil, err
	}
	if rs.Empty() {
		return nil, nil
	}
	return rs, nil
}

func compileDirection(in *Input, rs *Ruleset, direction string) (*Chain, error) {
	chain := &Chain{Direction: direction}

	admin := slices.Clone(in.Rules.AdminPolicies)
	structs.SortAdminNetworkPolicies(admin)
	for _, policy := range admin {
		for i, rule := range policy.Rules(direction) {
			if rule == nil {
				continue
			}
			ruleName := rule.Name
			if ruleName == "" {
				ruleName = fmt.Sprint(i)
			}
			compiled, ok, err := compileRule(in, "", direction, rule)
			if err != nil {
				return nil, fmt.Errorf("admin network policy %q: %w", policy.Name, err)
			}
			if !ok {
				continue
			}
			compiled.Origin = fmt.Sprintf("admin/%s/%s/%s", policy.Name, direction, ruleName)
			switch rule.Action {
			case structs.NetworkPolicyActionAllow:
				compiled.Verdict = VerdictAccept
			case structs.NetworkPolicyActionDeny:
				compiled.Verdict = VerdictDrop
				compiled.Counter = fmt.Sprintf("%s_admin_%d", direction, len(rs.Counters))
				rs.Counters = append(rs.Counters, &Counter{
					Name:      compiled.Counter,
					Direction: direction,
					Kind:      "admin",
					Policy:    policy.Name,
					Rule:      ruleName,
				})
			case structs.NetworkPolicyActionPass:
				compiled.Verdict = VerdictReturn
			default:
				return nil, fmt.Errorf("admin network policy %q: invalid action %q", policy.Name, rule.Action)
			}
			chain.Admin = append(chain.Admin, compiled)
		}
	}

	policies := slices.Clone(in.Rules.Policies)
	sort.Slice(policies, func(i, j int) bool { return policies[i].Name < policies[j].Name })
	var isolating []string
	for _, policy := range policies {
		if policy.Namespace != in.Namespace || !policy.Isolates(direction) {
			continue
		}
		isolating = append(isolating, policy.Name)
		for i, rule := range policy.Rules(direction) {
			if rule == nil {
				continue
			}
			compiled, ok, err := compileRule(in, policy.Namespace, direction, rule)
			if err != nil {
				return nil, fmt.Errorf("network policy %q: %w", policy.Name, err)
			}
			if !ok {
				continue
			}
			ruleName := rule.Name
			if ruleName == "" {
				ruleName = fmt.Sprint(i)
			}
			compiled.Verdict = VerdictAccept
			compiled.Origin = fmt.Sprintf("%s/%s/%s/%s", policy.Namespace, policy.Name, direction, ruleName)
			chain.Allow = append(chain.Allow, compiled)
		}
	}
	if len(isolating) > 0 {
		chain.Isolated = true
		chain.DefaultCounter = direction + "_default"
		rs.Counters = append(rs.Counters, &Counter{
			Name:      chain.DefaultCounter,
			Direction: direction,
			Kind:      "default-deny",
			Policy:    strings.Join(isolating, ","),
		})
	}

	if len(chain.Admin) == 0 && !chain.Isolated {
		return nil, nil
	}
	if direction == structs.NetworkPolicyDirectionIngress {
		chain.NodeAddrs = slices.Clone(in.NodeAddrs)
	}
	return chain, nil
}

// compileRule compiles the peers and ports of a rule. It returns false if
// the rule cannot match anything, e.g. because its allocation peers select
// no allocation of the node or its port labels do not exist.
func compileRule(in *Input, policyNamespace, direction string, rule *structs.NetworkPolicyRule) (*Rule, bool, error) {
	out := &Rule{}

	if len(rule.Peers) > 0 {
		out.Prefixes = []netip.Prefix{}
		for _, peer := range rule.Peers {
			if peer == nil {
				continue
			}
			if len(peer.CIDR) > 0 {
				for _, cidr := range peer.CIDR {
					prefix, err := netip.ParsePrefix(cidr)
					if err != nil {
						return nil, false, fmt.Errorf("invalid CIDR %q: %w", cidr, err)
					}
					out.Prefixes = append(out.Prefixes, prefix.Masked())
				}
				continue
			}
			for _, alloc := range in.Rules.Peers {
				if !peer.Matches(policyNamespace, alloc.Namespace, alloc.Meta) {
					continue
				}
				addr, err := netip.ParseAddr(alloc.Address)
				if err != nil {
					continue
				}
				addr = addr.Unmap()
				out.Prefixes = append(out.Prefixes, netip.PrefixFrom(addr, addr.BitLen()))
			}
		}
		out.Prefixes = dedupPrefixes(out.Prefixes)
		if len(out.Prefixes) == 0 {
			return nil, false, nil
		}
	}

	if len(rule.Ports) > 0 {
		out.Ports = []PortRange{}
		for _, spec := range rule.Ports {
			port, err := structs.ParseNetworkPolicyPort(spec)
			if err != nil {
				return nil, false, err
			}
			proto := protoNumber(port.Protocol)
			if port.Label == "" {
				out.Ports = append(out.Ports, PortRange{Proto: proto, From: port.From, To: port.To})
				continue
			}
			if direction != structs.NetworkPolicyDirectionIngress {
				return nil, false, fmt.Errorf("port label %q in an egress rule", port.Label)
			}
			if mapping, ok := in.Ports.Get(port.Label); ok {
				to := mapping.To
				if to < 1 {
					to = mapping.Value
				}
				if to > 0 && to <= 65535 {
					out.Ports = append(out.Ports, PortRange{Proto: proto, From: uint16(to), To: uint16(to)})
				}
			}
		}
		out.Ports = dedupPorts(out.Ports)
		if len(out.Ports) == 0 {
			return nil, false, nil
		}
	}
	return out, true, nil
}

func protoNumber(proto string) uint8 {
	switch proto {
	case "udp":
		return ProtoUDP
	case "sctp":
		return ProtoSCTP
	default:
		return ProtoTCP
	}
}

func protoName(proto uint8) string {
	switch proto {
	case ProtoUDP:
		return "udp"
	case ProtoSCTP:
		return "sctp"
	default:
		return "tcp"
	}
}

func dedupPrefixes(prefixes []netip.Prefix) []netip.Prefix {
	sort.Slice(prefixes, func(i, j int) bool {
		if c := prefixes[i].Addr().Compare(prefixes[j].Addr()); c != 0 {
			return c < 0
		}
		return prefixes[i].Bits() < prefixes[j].Bits()
	})
	return slices.Compact(prefixes)
}

func dedupPorts(ports []PortRange) []PortRange {
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].Proto != ports[j].Proto {
			return ports[i].Proto < ports[j].Proto
		}
		if ports[i].From != ports[j].From {
			return ports[i].From < ports[j].From
		}
		return ports[i].To < ports[j].To
	})
	return slices.Compact(ports)
}

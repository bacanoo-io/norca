// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package netpolicy

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/hashicorp/nomad/nomad/structs"
)

// match is a single nftables rule worth of matching: an optional peer
// prefix and an optional destination port range.
type match struct {
	prefix *netip.Prefix
	port   *PortRange
}

// expand returns the matches of the rule: the cross product of its prefixes
// and port ranges, one nftables rule each.
func (r *Rule) expand() []match {
	prefixes := []*netip.Prefix{nil}
	if r.Prefixes != nil {
		prefixes = prefixes[:0]
		for i := range r.Prefixes {
			prefixes = append(prefixes, &r.Prefixes[i])
		}
	}
	ports := []*PortRange{nil}
	if r.Ports != nil {
		ports = ports[:0]
		for i := range r.Ports {
			ports = append(ports, &r.Ports[i])
		}
	}
	out := make([]match, 0, len(prefixes)*len(ports))
	for _, prefix := range prefixes {
		for _, port := range ports {
			out = append(out, match{prefix: prefix, port: port})
		}
	}
	return out
}

// baseChainName, adminChainName and nsChainName name the chains of a
// direction.
func baseChainName(direction string) string  { return direction }
func adminChainName(direction string) string { return direction + "_admin" }
func nsChainName(direction string) string    { return direction + "_ns" }

// String renders the rule set in nft syntax. The output is deterministic
// and is used to detect changes, in logs and in tests; Apply programs the
// same rules through netlink.
func (r *Ruleset) String() string {
	if r.Empty() {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s {\n", TableName)
	for _, c := range r.Counters {
		fmt.Fprintf(&b, "\tcounter %s {\n\t}\n", c.Name)
	}
	for _, chain := range []*Chain{r.Ingress, r.Egress} {
		if chain != nil {
			chain.render(&b)
		}
	}
	b.WriteString("}\n")
	return b.String()
}

func (c *Chain) render(b *strings.Builder) {
	hook, ifname := "input", "iifname"
	if c.Direction == structs.NetworkPolicyDirectionEgress {
		hook, ifname = "output", "oifname"
	}
	fmt.Fprintf(b, "\tchain %s {\n", baseChainName(c.Direction))
	fmt.Fprintf(b, "\t\ttype filter hook %s priority filter; policy accept;\n", hook)
	fmt.Fprintf(b, "\t\t%s \"lo\" accept\n", ifname)
	b.WriteString("\t\tct state established,related accept\n")
	b.WriteString("\t\tct state invalid drop\n")
	b.WriteString("\t\tmeta l4proto ipv6-icmp icmpv6 type { nd-router-solicit, nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert } accept\n")
	for _, addr := range c.NodeAddrs {
		prefix := netip.PrefixFrom(addr, addr.BitLen())
		fmt.Fprintf(b, "\t\t%s accept\n", addrMatchText(c.Direction, prefix))
	}
	if len(c.Admin) > 0 {
		fmt.Fprintf(b, "\t\tjump %s\n", adminChainName(c.Direction))
	}
	if c.Isolated {
		fmt.Fprintf(b, "\t\tjump %s\n", nsChainName(c.Direction))
	}
	b.WriteString("\t}\n")

	if len(c.Admin) > 0 {
		fmt.Fprintf(b, "\tchain %s {\n", adminChainName(c.Direction))
		for _, rule := range c.Admin {
			rule.render(b, c.Direction)
		}
		b.WriteString("\t}\n")
	}
	if c.Isolated {
		fmt.Fprintf(b, "\tchain %s {\n", nsChainName(c.Direction))
		for _, rule := range c.Allow {
			rule.render(b, c.Direction)
		}
		fmt.Fprintf(b, "\t\tcounter name %q drop\n", c.DefaultCounter)
		b.WriteString("\t}\n")
	}
}

func (r *Rule) render(b *strings.Builder, direction string) {
	for _, m := range r.expand() {
		var parts []string
		if m.prefix != nil {
			parts = append(parts, addrMatchText(direction, *m.prefix))
		}
		if m.port != nil {
			parts = append(parts, portMatchText(*m.port))
		}
		if r.Counter != "" {
			parts = append(parts, fmt.Sprintf("counter name %q", r.Counter))
		}
		parts = append(parts, r.Verdict.String())
		if r.Origin != "" {
			parts = append(parts, fmt.Sprintf("comment %q", truncate(r.Origin, 127)))
		}
		fmt.Fprintf(b, "\t\t%s\n", strings.Join(parts, " "))
	}
}

// addrMatchText renders the peer address match of a direction: the source
// for ingress, the destination for egress. A /0 prefix only matches the
// address family.
func addrMatchText(direction string, prefix netip.Prefix) string {
	family := "ip"
	if prefix.Addr().Is6() {
		family = "ip6"
	}
	field := "saddr"
	if direction == structs.NetworkPolicyDirectionEgress {
		field = "daddr"
	}
	if prefix.Bits() == 0 {
		return fmt.Sprintf("meta nfproto %s", map[string]string{"ip": "ipv4", "ip6": "ipv6"}[family])
	}
	if prefix.Bits() == prefix.Addr().BitLen() {
		return fmt.Sprintf("%s %s %s", family, field, prefix.Addr())
	}
	return fmt.Sprintf("%s %s %s", family, field, prefix)
}

func portMatchText(p PortRange) string {
	if p.From == p.To {
		return fmt.Sprintf("%s dport %d", protoName(p.Proto), p.From)
	}
	return fmt.Sprintf("%s dport %d-%d", protoName(p.Proto), p.From, p.To)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package netpolicy

import (
	"errors"
	"fmt"
	"net/netip"
	"os"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/google/nftables/userdata"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// nftObjectCounter is NFT_OBJECT_COUNTER.
const nftObjectCounter = 1

// Supported returns an error if nftables cannot be used on the host, e.g.
// because the kernel has no nf_tables support or the process lacks
// CAP_NET_ADMIN.
func Supported() error {
	conn, err := nftables.New()
	if err != nil {
		return err
	}
	_, err = conn.ListTablesOfFamily(nftables.TableFamilyINet)
	return err
}

// openNetNS opens a netlink connection to the network namespace at nsPath
// (the current namespace if empty). The returned function releases it.
func openNetNS(nsPath string) (*nftables.Conn, func(), error) {
	if nsPath == "" {
		conn, err := nftables.New()
		return conn, func() {}, err
	}
	f, err := os.Open(nsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open network namespace %q: %w", nsPath, err)
	}
	conn, err := nftables.New(nftables.WithNetNSFd(int(f.Fd())))
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return conn, func() { f.Close() }, nil
}

// Apply atomically replaces the rule set of the network namespace at nsPath
// with rs. An empty rule set removes the table, which restores the
// unrestricted behavior.
func Apply(nsPath string, rs *Ruleset) error {
	conn, release, err := openNetNS(nsPath)
	if err != nil {
		return err
	}
	defer release()

	table := &nftables.Table{Name: TableName, Family: nftables.TableFamilyINet}
	// Adding an existing table is a no-op, so the delete always succeeds;
	// the whole batch is one transaction.
	conn.AddTable(table)
	conn.DelTable(table)
	if !rs.Empty() {
		conn.AddTable(table)
		for _, c := range rs.Counters {
			conn.AddObj(&nftables.CounterObj{Table: table, Name: c.Name})
		}
		for _, chain := range []*Chain{rs.Ingress, rs.Egress} {
			if chain != nil {
				addChain(conn, table, chain)
			}
		}
	}
	if err := conn.Flush(); err != nil {
		return fmt.Errorf("failed to program nftables rules: %w", err)
	}
	return nil
}

// ReadCounters returns the packet counts of the drop counters of the rule
// set in the network namespace at nsPath. It returns an empty map if there
// is no rule set.
func ReadCounters(nsPath string) (map[string]uint64, error) {
	conn, release, err := openNetNS(nsPath)
	if err != nil {
		return nil, err
	}
	defer release()

	table := &nftables.Table{Name: TableName, Family: nftables.TableFamilyINet}
	objs, err := conn.GetObjects(table)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return map[string]uint64{}, nil
		}
		return nil, err
	}
	out := make(map[string]uint64, len(objs))
	for _, obj := range objs {
		if c, ok := obj.(*nftables.CounterObj); ok {
			out[c.Name] = c.Packets
		}
	}
	return out, nil
}

func addChain(conn *nftables.Conn, table *nftables.Table, c *Chain) {
	hook, ifnameKey := nftables.ChainHookInput, expr.MetaKeyIIFNAME
	if c.Direction == structs.NetworkPolicyDirectionEgress {
		hook, ifnameKey = nftables.ChainHookOutput, expr.MetaKeyOIFNAME
	}
	policy := nftables.ChainPolicyAccept
	base := conn.AddChain(&nftables.Chain{
		Name:     baseChainName(c.Direction),
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  hook,
		Priority: nftables.ChainPriorityFilter,
		Policy:   &policy,
	})
	var admin, ns *nftables.Chain
	if len(c.Admin) > 0 {
		admin = conn.AddChain(&nftables.Chain{Name: adminChainName(c.Direction), Table: table})
	}
	if c.Isolated {
		ns = conn.AddChain(&nftables.Chain{Name: nsChainName(c.Direction), Table: table})
	}

	add := func(chain *nftables.Chain, exprs ...expr.Any) {
		conn.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: exprs})
	}

	// Loopback traffic.
	add(base,
		&expr.Meta{Key: ifnameKey, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname("lo")},
		&expr.Verdict{Kind: expr.VerdictAccept})
	// Replies and related traffic of allowed connections.
	add(base, ctStateExprs(expr.CtStateBitESTABLISHED|expr.CtStateBitRELATED, expr.VerdictAccept)...)
	add(base, ctStateExprs(expr.CtStateBitINVALID, expr.VerdictDrop)...)
	// IPv6 neighbor discovery (ICMPv6 types 133-136).
	add(base,
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{ProtoICMPv6}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 0, Len: 1},
		&expr.Range{Op: expr.CmpOpEq, Register: 1, FromData: []byte{133}, ToData: []byte{136}},
		&expr.Verdict{Kind: expr.VerdictAccept})
	// The node itself.
	for _, addr := range c.NodeAddrs {
		exprs := addrExprs(c.Direction, netip.PrefixFrom(addr, addr.BitLen()))
		add(base, append(exprs, &expr.Verdict{Kind: expr.VerdictAccept})...)
	}
	if admin != nil {
		add(base, &expr.Verdict{Kind: expr.VerdictJump, Chain: admin.Name})
		for _, rule := range c.Admin {
			addRule(conn, table, admin, c.Direction, rule)
		}
	}
	if ns != nil {
		add(base, &expr.Verdict{Kind: expr.VerdictJump, Chain: ns.Name})
		for _, rule := range c.Allow {
			addRule(conn, table, ns, c.Direction, rule)
		}
		add(ns,
			&expr.Objref{Type: nftObjectCounter, Name: c.DefaultCounter},
			&expr.Verdict{Kind: expr.VerdictDrop})
	}
}

func addRule(conn *nftables.Conn, table *nftables.Table, chain *nftables.Chain, direction string, rule *Rule) {
	var udata []byte
	if rule.Origin != "" {
		udata = userdata.AppendString(nil, userdata.TypeComment, truncate(rule.Origin, 127))
	}
	for _, m := range rule.expand() {
		var exprs []expr.Any
		if m.prefix != nil {
			exprs = append(exprs, addrExprs(direction, *m.prefix)...)
		}
		if m.port != nil {
			exprs = append(exprs, portExprs(*m.port)...)
		}
		if rule.Counter != "" {
			exprs = append(exprs, &expr.Objref{Type: nftObjectCounter, Name: rule.Counter})
		}
		exprs = append(exprs, &expr.Verdict{Kind: verdictKind(rule.Verdict)})
		conn.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: exprs, UserData: udata})
	}
}

func verdictKind(v Verdict) expr.VerdictKind {
	switch v {
	case VerdictAccept:
		return expr.VerdictAccept
	case VerdictDrop:
		return expr.VerdictDrop
	default:
		return expr.VerdictReturn
	}
}

// ctStateExprs matches the conntrack state bits.
func ctStateExprs(bits uint32, verdict expr.VerdictKind) []expr.Any {
	return []expr.Any{
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{
			SourceRegister: 1,
			DestRegister:   1,
			Len:            4,
			Mask:           binaryutil.NativeEndian.PutUint32(bits),
			Xor:            binaryutil.NativeEndian.PutUint32(0),
		},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: binaryutil.NativeEndian.PutUint32(0)},
		&expr.Verdict{Kind: verdict},
	}
}

// addrExprs matches the address family and the peer address: the source
// for ingress, the destination for egress.
func addrExprs(direction string, prefix netip.Prefix) []expr.Any {
	addr := prefix.Addr()
	family, offset, length := byte(unix.NFPROTO_IPV4), uint32(12), uint32(4)
	if addr.Is6() {
		family, offset, length = byte(unix.NFPROTO_IPV6), 8, 16
	}
	if direction == structs.NetworkPolicyDirectionEgress {
		offset += length
	}
	exprs := []expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{family}},
	}
	if prefix.Bits() == 0 {
		return exprs
	}
	exprs = append(exprs, &expr.Payload{
		DestRegister: 1,
		Base:         expr.PayloadBaseNetworkHeader,
		Offset:       offset,
		Len:          length,
	})
	if prefix.Bits() < addr.BitLen() {
		mask := netip.PrefixFrom(allOnes(addr.Is6()), prefix.Bits()).Masked().Addr().AsSlice()
		exprs = append(exprs, &expr.Bitwise{
			SourceRegister: 1,
			DestRegister:   1,
			Len:            length,
			Mask:           mask,
			Xor:            make([]byte, length),
		})
	}
	return append(exprs, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: prefix.Masked().Addr().AsSlice()})
}

func allOnes(v6 bool) netip.Addr {
	if v6 {
		var b [16]byte
		for i := range b {
			b[i] = 0xff
		}
		return netip.AddrFrom16(b)
	}
	return netip.AddrFrom4([4]byte{0xff, 0xff, 0xff, 0xff})
}

// portExprs matches the transport protocol and the destination port.
func portExprs(p PortRange) []expr.Any {
	exprs := []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{p.Proto}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
	}
	if p.From == p.To {
		return append(exprs, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(p.From)})
	}
	return append(exprs, &expr.Range{
		Op:       expr.CmpOpEq,
		Register: 1,
		FromData: binaryutil.BigEndian.PutUint16(p.From),
		ToData:   binaryutil.BigEndian.PutUint16(p.To),
	})
}

// Gateways returns the gateways of the default routes of the network
// namespace at nsPath. In bridge mode this is the address of the bridge on
// the node, the source of traffic from the node itself.
func Gateways(nsPath string) ([]netip.Addr, error) {
	ns, err := netns.GetFromPath(nsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open network namespace %q: %w", nsPath, err)
	}
	defer ns.Close()
	handle, err := netlink.NewHandleAt(ns)
	if err != nil {
		return nil, err
	}
	defer handle.Close()

	routes, err := handle.RouteList(nil, netlink.FAMILY_ALL)
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, route := range routes {
		if route.Gw == nil {
			continue
		}
		if route.Dst != nil {
			if ones, _ := route.Dst.Mask.Size(); ones != 0 {
				continue
			}
		}
		if addr, ok := netip.AddrFromSlice(route.Gw); ok {
			out = append(out, addr.Unmap())
		}
	}
	return out, nil
}

// ifname returns an interface name as the kernel compares it.
func ifname(name string) []byte {
	b := make([]byte, unix.IFNAMSIZ)
	copy(b, name)
	return b
}

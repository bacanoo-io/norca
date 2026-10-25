// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package netpolicy

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// The tests in this file program nftables in throw-away network namespaces
// and need root. They only run with NORCA_NFT_ROOT_TESTS=1 as root; they
// never touch the network namespace of the host.

func requireRootNftTests(t *testing.T) {
	t.Helper()
	if os.Getenv("NORCA_NFT_ROOT_TESTS") != "1" {
		t.Skip("set NORCA_NFT_ROOT_TESTS=1 and run as root to run the nftables integration tests")
	}
	if os.Geteuid() != 0 {
		t.Skip("the nftables integration tests need root")
	}
}

// testNetNS is a network namespace connected to a peer namespace with a
// veth pair.
type testNetNS struct {
	handle netns.NsHandle
	path   string
}

// inNS runs f with the calling goroutine's thread in the namespace.
func inNS(t *testing.T, ns netns.NsHandle, f func()) {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	must.NoError(t, err)
	defer orig.Close()
	must.NoError(t, netns.Set(ns))
	defer func() { must.NoError(t, netns.Set(orig)) }()
	f()
}

// newNSPair creates two namespaces linked by a veth pair: a (10.99.0.1)
// and b (10.99.0.2).
func newNSPair(t *testing.T) (a, b *testNetNS) {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	orig, err := netns.Get()
	must.NoError(t, err)
	defer orig.Close()

	mk := func() *testNetNS {
		h, err := netns.New() // switches the thread
		must.NoError(t, err)
		must.NoError(t, netns.Set(orig))
		t.Cleanup(func() { h.Close() })
		return &testNetNS{handle: h, path: fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), int(h))}
	}
	a, b = mk(), mk()

	ha, err := netlink.NewHandleAt(a.handle)
	must.NoError(t, err)
	defer ha.Close()
	hb, err := netlink.NewHandleAt(b.handle)
	must.NoError(t, err)
	defer hb.Close()

	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "npa"}, PeerName: "npb"}
	must.NoError(t, ha.LinkAdd(veth))
	peer, err := ha.LinkByName("npb")
	must.NoError(t, err)
	must.NoError(t, ha.LinkSetNsFd(peer, int(b.handle)))

	setup := func(h *netlink.Handle, name, addr string) {
		link, err := h.LinkByName(name)
		must.NoError(t, err)
		ip, err := netlink.ParseAddr(addr)
		must.NoError(t, err)
		must.NoError(t, h.AddrAdd(link, ip))
		must.NoError(t, h.LinkSetUp(link))
		lo, err := h.LinkByName("lo")
		must.NoError(t, err)
		must.NoError(t, h.LinkSetUp(lo))
	}
	setup(ha, "npa", "10.99.0.1/24")
	setup(hb, "npb", "10.99.0.2/24")
	return a, b
}

// listen starts TCP listeners in the namespace that accept and close
// connections.
func listen(t *testing.T, ns *testNetNS, ports ...int) {
	t.Helper()
	for _, port := range ports {
		var l net.Listener
		inNS(t, ns.handle, func() {
			var err error
			l, err = net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
			must.NoError(t, err)
		})
		t.Cleanup(func() { l.Close() })
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
	}
}

// dial reports whether a TCP connection from the namespace succeeds.
func dial(t *testing.T, ns *testNetNS, addr string) bool {
	t.Helper()
	var ok bool
	inNS(t, ns.handle, func() {
		c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			c.Close()
			ok = true
		}
	})
	return ok
}

func TestNft_EgressRules(t *testing.T) {
	requireRootNftTests(t)

	a, b := newNSPair(t)
	listen(t, b, 8001, 8002)
	must.True(t, dial(t, a, "10.99.0.2:8001"))
	must.True(t, dial(t, a, "10.99.0.2:8002"))

	rs, err := Compile(&Input{Namespace: "default", Rules: &structs.NetworkPolicyAllocRules{
		Policies: []*structs.NetworkPolicy{{
			Name: "egress", Namespace: "default",
			Egress: []*structs.NetworkPolicyRule{{
				Peers: []*structs.NetworkPolicyPeer{{CIDR: []string{"10.99.0.0/24"}}},
				Ports: []string{"8001/tcp"},
			}},
		}},
	}})
	must.NoError(t, err)
	must.NoError(t, Apply(a.path, rs))

	// The allowed destination works, the other one is dropped and counted.
	must.True(t, dial(t, a, "10.99.0.2:8001"))
	must.False(t, dial(t, a, "10.99.0.2:8002"))
	counts, err := ReadCounters(a.path)
	must.NoError(t, err)
	must.Positive(t, counts["egress_default"])

	// Applying again replaces the rules atomically.
	must.NoError(t, Apply(a.path, rs))

	// Removing the rule set restores the unrestricted behavior.
	must.NoError(t, Apply(a.path, nil))
	must.True(t, dial(t, a, "10.99.0.2:8002"))
	counts, err = ReadCounters(a.path)
	must.NoError(t, err)
	must.MapEmpty(t, counts)
}

func TestNft_IngressAdminDeny(t *testing.T) {
	requireRootNftTests(t)

	a, b := newNSPair(t)
	listen(t, b, 9001, 9002)

	rs, err := Compile(&Input{
		Namespace: "default",
		Rules: &structs.NetworkPolicyAllocRules{
			// The namespace policy allows everything on 9001 and 9002...
			Policies: []*structs.NetworkPolicy{{
				Name: "ingress", Namespace: "default",
				Ingress: []*structs.NetworkPolicyRule{{Ports: []string{"9001-9002/tcp"}}},
			}},
			// ...but the admin policy denies 9002 from a.
			AdminPolicies: []*structs.AdminNetworkPolicy{{
				Name: "deny",
				Ingress: []*structs.NetworkPolicyRule{{
					Name:   "a",
					Action: structs.NetworkPolicyActionDeny,
					Peers:  []*structs.NetworkPolicyPeer{{CIDR: []string{"10.99.0.1/32"}}},
					Ports:  []string{"9002/tcp"},
				}},
			}},
		},
	})
	must.NoError(t, err)
	must.NoError(t, Apply(b.path, rs))

	must.True(t, dial(t, a, "10.99.0.2:9001"))
	must.False(t, dial(t, a, "10.99.0.2:9002"))
	counts, err := ReadCounters(b.path)
	must.NoError(t, err)
	must.Positive(t, counts["ingress_admin_0"])
}

func TestNft_Gateways(t *testing.T) {
	requireRootNftTests(t)

	a, _ := newNSPair(t)
	ha, err := netlink.NewHandleAt(a.handle)
	must.NoError(t, err)
	defer ha.Close()
	link, err := ha.LinkByName("npa")
	must.NoError(t, err)
	must.NoError(t, ha.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.ParseIP("10.99.0.2")}))

	gws, err := Gateways(a.path)
	must.NoError(t, err)
	must.Eq(t, []netip.Addr{netip.MustParseAddr("10.99.0.2")}, gws)
}

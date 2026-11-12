// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package allocrunner

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/shoenig/test/must"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/client/testutil"
)

// A new namespace has only lo. An interface that is down and has no address,
// as the fallback tunnel devices of the kernel are, does not make it a
// configured one; an interface that is up, or has an address, does.
func TestNamespaceConfigured(t *testing.T) {
	ci.Parallel(t)
	testutil.RequireRoot(t)

	// Creating a namespace switches the thread into it: do it on a locked
	// thread and switch back.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	must.NoError(t, err)
	defer orig.Close()
	ns, err := netns.New()
	must.NoError(t, err)
	defer ns.Close()
	must.NoError(t, netns.Set(orig))

	path := fmt.Sprintf("/proc/self/fd/%d", int(ns))

	configured, err := namespaceConfigured(path)
	must.NoError(t, err)
	must.False(t, configured)

	handle, err := netlink.NewHandleAt(ns)
	must.NoError(t, err)
	defer handle.Close()
	must.NoError(t, handle.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "tunl0"}}))

	configured, err = namespaceConfigured(path)
	must.NoError(t, err)
	must.False(t, configured)

	// An address, on an interface that is down.
	tunnel, err := handle.LinkByName("tunl0")
	must.NoError(t, err)
	addr, err := netlink.ParseAddr("172.26.64.9/20")
	must.NoError(t, err)
	must.NoError(t, handle.AddrAdd(tunnel, addr))
	configured, err = namespaceConfigured(path)
	must.NoError(t, err)
	must.True(t, configured)
	must.NoError(t, handle.AddrDel(tunnel, addr))

	// An interface that is up.
	must.NoError(t, handle.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0"}}))
	eth, err := handle.LinkByName("eth0")
	must.NoError(t, err)
	configured, err = namespaceConfigured(path)
	must.NoError(t, err)
	must.False(t, configured)
	must.NoError(t, handle.LinkSetUp(eth))
	configured, err = namespaceConfigured(path)
	must.NoError(t, err)
	must.True(t, configured)

	_, err = namespaceConfigured("/nonexistent/netns")
	must.Error(t, err)
}

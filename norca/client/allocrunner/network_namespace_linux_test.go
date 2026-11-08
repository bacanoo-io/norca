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

// A new namespace has only lo; one with another interface is configured.
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
	must.NoError(t, handle.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0"}}))

	configured, err = namespaceConfigured(path)
	must.NoError(t, err)
	must.True(t, configured)

	_, err = namespaceConfigured("/nonexistent/netns")
	must.Error(t, err)
}

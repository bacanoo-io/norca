// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package allocrunner

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// namespaceConfigured reports whether the network of an alloc was set up in
// the network namespace at path: whether it has an interface that says so
// (isConfiguredInterface). A namespace that was configured for an alloc has
// at least the interface of its network (eth0 of the bridge), up and with
// its address; a new one has lo, and the fallback tunnel devices of the
// kernel where their modules are loaded.
func namespaceConfigured(path string) (bool, error) {
	ns, err := netns.GetFromPath(path)
	if err != nil {
		return false, fmt.Errorf("opening the namespace: %w", err)
	}
	defer ns.Close()

	handle, err := netlink.NewHandleAt(ns)
	if err != nil {
		return false, fmt.Errorf("opening a netlink handle in the namespace: %w", err)
	}
	defer handle.Close()

	links, err := handle.LinkList()
	if err != nil {
		return false, fmt.Errorf("listing the interfaces of the namespace: %w", err)
	}
	for _, link := range links {
		flags := link.Attrs().Flags
		if flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := handle.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			return false, fmt.Errorf("listing the addresses of %s in the namespace: %w", link.Attrs().Name, err)
		}
		if isConfiguredInterface(flags, len(addrs)) {
			return true, nil
		}
	}
	return false, nil
}

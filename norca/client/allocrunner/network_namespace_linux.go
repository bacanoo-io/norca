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

// namespaceConfigured reports whether the network namespace at path has an
// interface besides the loopback one. A namespace that was configured for an
// alloc has at least the interface of its network (eth0 of the bridge); a new
// one has only lo.
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
		if link.Attrs().Flags&net.FlagLoopback == 0 {
			return true, nil
		}
	}
	return false, nil
}

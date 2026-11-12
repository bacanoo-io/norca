// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package allocrunner

import "net"

// isConfiguredInterface reports whether an interface of a network namespace
// shows that the network of an alloc was set up in it: it is not the
// loopback interface, and it is up or has an address.
//
// "Not the loopback" alone is not enough. With the ipip, sit, ip6_tunnel or
// gre module loaded, the kernel puts fallback tunnel devices (tunl0, sit0,
// ip6tnl0, gre0, ...) into every new namespace, unless
// net.core.fb_tunnels_only_for_init_net says otherwise: down, and without
// an address. A namespace that has only those and lo is as empty as one
// that has only lo. What a CNI plugin sets up is up; an interface that has
// an address was given one by somebody.
func isConfiguredInterface(flags net.Flags, addresses int) bool {
	if flags&net.FlagLoopback != 0 {
		return false
	}
	return flags&net.FlagUp != 0 || addresses > 0
}

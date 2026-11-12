// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package allocrunner

import (
	"net"
	"testing"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
)

func TestIsConfiguredInterface(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name      string
		flags     net.Flags
		addresses int
		expected  bool
	}{
		{name: "the loopback interface", flags: net.FlagLoopback | net.FlagUp, addresses: 2, expected: false},
		{name: "a fallback tunnel device: down, no address", flags: 0, addresses: 0, expected: false},
		{name: "the interface of the network: up, with its address", flags: net.FlagUp | net.FlagBroadcast, addresses: 1, expected: true},
		{name: "up without an address (a network without address management)", flags: net.FlagUp, addresses: 0, expected: true},
		{name: "down with an address", flags: net.FlagBroadcast, addresses: 1, expected: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must.Eq(t, tt.expected, isConfiguredInterface(tt.flags, tt.addresses))
		})
	}
}

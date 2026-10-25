// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build !linux

package netpolicy

import (
	"errors"
	"net/netip"
)

// ErrUnsupported is returned on platforms without nftables.
var ErrUnsupported = errors.New("network policies are only enforced on Linux")

// Supported returns ErrUnsupported.
func Supported() error { return ErrUnsupported }

// Apply returns ErrUnsupported unless the rule set is empty.
func Apply(_ string, rs *Ruleset) error {
	if rs.Empty() {
		return nil
	}
	return ErrUnsupported
}

// ReadCounters returns no counters.
func ReadCounters(string) (map[string]uint64, error) { return map[string]uint64{}, nil }

// Gateways returns no addresses.
func Gateways(string) ([]netip.Addr, error) { return nil, nil }

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package defaultroute

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// The routing tables of the kernel; replaced in tests.
var (
	ipv4Table = "/proc/net/route"
	ipv6Table = "/proc/net/ipv6_route"
)

// InterfaceName returns the name of the interface the default route leaves
// through: the IPv4 default route, or the IPv6 one on a host without an
// IPv4 default route. It returns ErrNoDefaultRoute if there is neither.
func InterfaceName() (string, error) {
	name, err := fromTable(ipv4Table, parseIPv4)
	if err == nil {
		return name, nil
	}
	if !errors.Is(err, ErrNoDefaultRoute) && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	name, err = fromTable(ipv6Table, parseIPv6)
	if err == nil {
		return name, nil
	}
	if errors.Is(err, ErrNoDefaultRoute) || errors.Is(err, os.ErrNotExist) {
		return "", ErrNoDefaultRoute
	}
	return "", err
}

func fromTable(path string, parse func(io.Reader) (string, error)) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("the routing table of the kernel: %w", err)
	}
	defer f.Close()
	return parse(f)
}

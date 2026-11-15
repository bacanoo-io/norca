// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package defaultroute finds the network interface of the default route.
//
// The agent needs it when nothing names an interface: the client
// fingerprints the interface the default route leaves through, and an
// advertise address is preferred on it. On Linux the routing table is read
// from the kernel (/proc/net/route, /proc/net/ipv6_route), so no program of
// the host is run; go-sockaddr, which the agent used for this before, runs
// "ip route" and fails on a host or in an image without iproute2. On the
// other systems go-sockaddr is still asked.
package defaultroute

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ErrNoDefaultRoute says that the host has no default route.
var ErrNoDefaultRoute = errors.New("no default route found")

// The flags of a route in /proc/net/route and /proc/net/ipv6_route
// (include/uapi/linux/route.h, include/uapi/linux/ipv6_route.h).
const (
	flagUp     = 0x0001
	flagReject = 0x0200
)

// parseIPv4 reads the content of /proc/net/route and returns the interface
// of the default route that is up: the one with the lowest metric, the
// first of the table among equals. The table has a header line and one
// route per line:
//
//	Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT
//
// with addresses and flags in hexadecimal. A default route has the
// destination and the mask 00000000.
func parseIPv4(r io.Reader) (string, error) {
	best, bestMetric := "", uint64(0)
	scanner := bufio.NewScanner(r)
	for line := 0; scanner.Scan(); line++ {
		fields := strings.Fields(scanner.Text())
		if line == 0 || len(fields) < 8 {
			continue
		}
		iface, destination, mask := fields[0], fields[1], fields[7]
		if destination != "00000000" || mask != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil || flags&flagUp == 0 || flags&flagReject != 0 {
			continue
		}
		metric, err := strconv.ParseUint(fields[6], 10, 32)
		if err != nil {
			continue
		}
		if best == "" || metric < bestMetric {
			best, bestMetric = iface, metric
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("reading the routing table: %w", err)
	}
	if best == "" {
		return "", ErrNoDefaultRoute
	}
	return best, nil
}

// parseIPv6 reads the content of /proc/net/ipv6_route and returns the
// interface of the default route that is up, as parseIPv4 does. One route
// per line, without a header:
//
//	destination prefix source prefix next-hop metric refcnt use flags iface
//
// all but the interface in hexadecimal. A default route has the destination
// of 32 zeros and the prefix length 00. A host without an IPv6 default
// route has one that rejects (on lo), which is not one.
func parseIPv6(r io.Reader) (string, error) {
	best, bestMetric := "", uint64(0)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 {
			continue
		}
		if strings.Trim(fields[0], "0") != "" || len(fields[0]) != 32 || fields[1] != "00" {
			continue
		}
		flags, err := strconv.ParseUint(fields[8], 16, 32)
		if err != nil || flags&flagUp == 0 || flags&flagReject != 0 {
			continue
		}
		metric, err := strconv.ParseUint(fields[5], 16, 32)
		if err != nil {
			continue
		}
		if iface := fields[9]; best == "" || metric < bestMetric {
			best, bestMetric = iface, metric
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("reading the IPv6 routing table: %w", err)
	}
	if best == "" {
		return "", ErrNoDefaultRoute
	}
	return best, nil
}

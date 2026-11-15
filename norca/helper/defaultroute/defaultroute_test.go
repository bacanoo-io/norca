// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package defaultroute

import (
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

const header = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"

func TestParseIPv4(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name     string
		table    string
		expected string
		err      error
	}{
		{
			name: "one default route with a gateway",
			table: header +
				"eth0\t00000000\t0101A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
				"eth0\t0001A8C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n",
			expected: "eth0",
		},
		{
			name: "the default route is not the first line",
			table: header +
				"docker0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n" +
				"lab0\t00004D0A\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n" +
				"eth0\t00000000\t0101A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n",
			expected: "eth0",
		},
		{
			name: "the lowest metric of two default routes",
			table: header +
				"wlan0\t00000000\t0100000A\t0003\t0\t0\t600\t00000000\t0\t0\t0\n" +
				"eth0\t00000000\t0101A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n",
			expected: "eth0",
		},
		{
			name: "the first of two default routes with the same metric",
			table: header +
				"eth1\t00000000\t0100000A\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
				"eth0\t00000000\t0101A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n",
			expected: "eth1",
		},
		{
			name: "a default route without a gateway (point to point)",
			table: header +
				"ppp0\t00000000\t00000000\t0001\t0\t0\t0\t00000000\t0\t0\t0\n",
			expected: "ppp0",
		},
		{
			name: "a default route that is down or rejects is none",
			table: header +
				"eth0\t00000000\t0101A8C0\t0002\t0\t0\t0\t00000000\t0\t0\t0\n" +
				"eth1\t00000000\t00000000\t0201\t0\t0\t0\t00000000\t0\t0\t0\n",
			err: ErrNoDefaultRoute,
		},
		{
			name: "a route to a network is not a default route",
			table: header +
				"eth0\t00000000\t00000000\t0001\t0\t0\t0\t000000FF\t0\t0\t0\n" +
				"eth0\t0000000A\t00000000\t0001\t0\t0\t0\t00000000\t0\t0\t0\n",
			err: ErrNoDefaultRoute,
		},
		{name: "only the header", table: header, err: ErrNoDefaultRoute},
		{name: "an empty table", table: "", err: ErrNoDefaultRoute},
		{
			name: "lines that cannot be read are skipped",
			table: header +
				"eth9\t00000000\t0101A8C0\tzz\t0\t0\t0\t00000000\t0\t0\t0\n" +
				"short line\n" +
				"eth0\t00000000\t0101A8C0\t0003\t0\t0\t5\t00000000\t0\t0\t0\n",
			expected: "eth0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseIPv4(strings.NewReader(tt.table))
			if tt.err != nil {
				must.ErrorIs(t, err, tt.err)
				return
			}
			must.NoError(t, err)
			must.Eq(t, tt.expected, got)
		})
	}
}

func TestParseIPv6(t *testing.T) {
	ci.Parallel(t)

	const (
		zero     = "00000000000000000000000000000000"
		linkLoc  = "fe800000000000000000000000000000"
		gateway  = "fe800000000000000000000000000001"
		rejected = zero + " 00 " + zero + " 00 " + zero + " ffffffff 00000001 00000000 00200200       lo\n"
	)
	tests := []struct {
		name     string
		table    string
		expected string
		err      error
	}{
		{
			name: "a default route next to the rejecting one",
			table: linkLoc + " 40 " + zero + " 00 " + zero + " 00000100 00000001 00000000 00000001     eth0\n" +
				zero + " 00 " + zero + " 00 " + gateway + " 00000400 00000002 00000000 00450003     eth0\n" +
				rejected,
			expected: "eth0",
		},
		{
			name: "the lowest metric (hexadecimal)",
			table: zero + " 00 " + zero + " 00 " + gateway + " 00000400 00000002 00000000 00000003     eth1\n" +
				zero + " 00 " + zero + " 00 " + gateway + " 00000064 00000002 00000000 00000003     eth0\n",
			expected: "eth0",
		},
		{name: "only the rejecting default route", table: rejected, err: ErrNoDefaultRoute},
		{
			name:  "a route to a prefix is not a default route",
			table: linkLoc + " 40 " + zero + " 00 " + zero + " 00000100 00000001 00000000 00000001     eth0\n",
			err:   ErrNoDefaultRoute,
		},
		{name: "an empty table", table: "", err: ErrNoDefaultRoute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseIPv6(strings.NewReader(tt.table))
			if tt.err != nil {
				must.ErrorIs(t, err, tt.err)
				return
			}
			must.NoError(t, err)
			must.Eq(t, tt.expected, got)
		})
	}
}

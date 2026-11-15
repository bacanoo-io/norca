// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package defaultroute

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/shoenig/test/must"
)

// The tables are read from files: IPv4 first, IPv6 when IPv4 has no default
// route or no table, and neither is ErrNoDefaultRoute.
func TestInterfaceName_Tables(t *testing.T) {
	dir := t.TempDir()
	v4, v6 := filepath.Join(dir, "route"), filepath.Join(dir, "ipv6_route")
	oldV4, oldV6 := ipv4Table, ipv6Table
	ipv4Table, ipv6Table = v4, v6
	t.Cleanup(func() { ipv4Table, ipv6Table = oldV4, oldV6 })

	const (
		zero    = "00000000000000000000000000000000"
		gateway = "fe800000000000000000000000000001"
	)
	withDefault := header + "eth0\t00000000\t0101A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"
	v6Default := zero + " 00 " + zero + " 00 " + gateway + " 00000400 00000002 00000000 00000003     eth6\n"

	// Neither table exists.
	_, err := InterfaceName()
	must.ErrorIs(t, err, ErrNoDefaultRoute)

	must.NoError(t, os.WriteFile(v4, []byte(header), 0o600))
	_, err = InterfaceName()
	must.ErrorIs(t, err, ErrNoDefaultRoute)

	must.NoError(t, os.WriteFile(v6, []byte(v6Default), 0o600))
	name, err := InterfaceName()
	must.NoError(t, err)
	must.Eq(t, "eth6", name)

	must.NoError(t, os.WriteFile(v4, []byte(withDefault), 0o600))
	name, err = InterfaceName()
	must.NoError(t, err)
	must.Eq(t, "eth0", name)
}

// On the host of the test the answer, if there is one, is an interface of
// the host.
func TestInterfaceName_Host(t *testing.T) {
	name, err := InterfaceName()
	if err != nil {
		must.ErrorIs(t, err, ErrNoDefaultRoute)
		t.Skip("the host of the test has no default route")
	}
	_, err = net.InterfaceByName(name)
	must.NoError(t, err)
}

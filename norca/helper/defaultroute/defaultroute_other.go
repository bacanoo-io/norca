// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build !linux

package defaultroute

import (
	sockaddr "github.com/hashicorp/go-sockaddr"
)

// InterfaceName returns the name of the interface the default route leaves
// through, as go-sockaddr finds it on this system.
func InterfaceName() (string, error) {
	ri, err := sockaddr.NewRouteInfo()
	if err != nil {
		return "", err
	}
	name, err := ri.GetDefaultInterfaceName()
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", ErrNoDefaultRoute
	}
	return name, nil
}

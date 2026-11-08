// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build !linux

package allocrunner

// namespaceConfigured: shared network namespaces exist only on Linux; an
// existing network is taken as configured elsewhere.
func namespaceConfigured(string) (bool, error) {
	return true, nil
}

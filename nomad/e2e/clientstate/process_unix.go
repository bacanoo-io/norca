// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build !windows

package clientstate

import "syscall"

// processExists returns an error if no process with the given pid exists.
func processExists(pid int) error {
	return syscall.Kill(pid, 0)
}

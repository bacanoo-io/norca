// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build windows

package clientstate

import "os"

// processExists returns an error if no process with the given pid exists. On
// Windows FindProcess opens a handle to the process, which fails if it is gone.
func processExists(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Release()
}

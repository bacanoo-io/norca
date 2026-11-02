// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package util

import "runtime"

// IsLinuxOS returns true if the operating system is some Linux distribution.
func IsLinuxOS() bool {
	return runtime.GOOS == "linux"
}

// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package testutil

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Copy of ci/slow.go for API.

// SkipSlow skips a slow test unless NORCA_SLOW_TEST is set to a true value.
func SkipSlow(t *testing.T, reason string) {
	value := os.Getenv("NORCA_SLOW_TEST")
	run, err := strconv.ParseBool(value)
	if !run || err != nil {
		t.Skipf("Skipping slow test: %s", reason)
	}
}

// Parallel runs t in parallel.
//
// The API package has been vetted to be concurrency safe (ish).
func Parallel(t *testing.T) {
	t.Parallel() // :)
}

func RequireRoot(t *testing.T) {
	t.Helper()
	if syscall.Getuid() != 0 {
		t.Skip("test requires root")
	}
}

// capSysAdmin is the bit of CAP_SYS_ADMIN in the capability sets of
// /proc/self/status.
const capSysAdmin = 21

// RequireMount skips a test whose task must start on a Linux client running
// as root when the process may not mount: such a client mounts a tmpfs on the
// secrets dir of every task, and without CAP_SYS_ADMIN (a container started
// without --cap-add SYS_ADMIN) the task fails and its allocation is
// rescheduled.
func RequireMount(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		return
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skipf("test requires CAP_SYS_ADMIN, the capabilities can't be read: %v", err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		hex, ok := strings.CutPrefix(line, "CapEff:")
		if !ok {
			continue
		}
		caps, err := strconv.ParseUint(strings.TrimSpace(hex), 16, 64)
		if err != nil {
			t.Skipf("test requires CAP_SYS_ADMIN, the capabilities can't be parsed: %v", err)
		}
		if caps&(1<<capSysAdmin) == 0 {
			t.Skip("test requires CAP_SYS_ADMIN to mount the task secrets dir (in a container: --cap-add SYS_ADMIN)")
		}
		return
	}
	t.Skip("test requires CAP_SYS_ADMIN, CapEff not found in /proc/self/status")
}

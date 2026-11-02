// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package shim

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shoenig/test/must"
)

// fakeCgroup makes a directory with the given (empty) cgroup files, like the
// kernel provides them.
func fakeCgroup(t *testing.T, files ...string) string {
	dir := t.TempDir()
	for _, f := range files {
		must.NoError(t, os.WriteFile(filepath.Join(dir, f), nil, 0o600))
	}
	return dir
}

func readFile(t *testing.T, dir, file string) string {
	b, err := os.ReadFile(filepath.Join(dir, file))
	must.NoError(t, err)
	return string(b)
}

var cgroupFiles = []string{"cpu.max", "memory.max", "memory.low", "memory.swap.max", "memory.oom.group", "pids.max"}

func TestConstrain_defaults(t *testing.T) {
	dir := fakeCgroup(t, cgroupFiles...)
	e := &exe{env: &Environment{Cgroup: dir, Memory: 256 << 20, CPUBandwidth: 50000}}
	must.NoError(t, e.constrain())

	must.Eq(t, "50000 100000", readFile(t, dir, "cpu.max"))
	must.Eq(t, "268435456", readFile(t, dir, "memory.max"))
	must.Eq(t, "0", readFile(t, dir, "memory.swap.max"))
	must.Eq(t, "1", readFile(t, dir, "memory.oom.group"))
	must.Eq(t, "", readFile(t, dir, "pids.max"), must.Sprint("no pids limit by default"))
}

func TestConstrain_pidsLimitAndSwap(t *testing.T) {
	dir := fakeCgroup(t, cgroupFiles...)
	e := &exe{env: &Environment{Cgroup: dir, Memory: 1 << 20, PidsLimit: 64, AllowSwap: true}}
	must.NoError(t, e.constrain())

	must.Eq(t, "64", readFile(t, dir, "pids.max"))
	must.Eq(t, "", readFile(t, dir, "memory.swap.max"), must.Sprint("swap allowed: left alone"))
	must.Eq(t, "1", readFile(t, dir, "memory.oom.group"))
}

func TestConstrain_noSwapAccounting(t *testing.T) {
	// without swap accounting the kernel has no memory.swap.max
	dir := fakeCgroup(t, "cpu.max", "memory.max", "memory.oom.group")
	e := &exe{env: &Environment{Cgroup: dir, Memory: 1 << 20}}
	must.NoError(t, e.constrain())
	must.Eq(t, "1", readFile(t, dir, "memory.oom.group"))
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package externaltest builds the test plugin of the external package for
// tests.
package externaltest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// BinaryName returns the file name of a plugin binary for the current OS.
func BinaryName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// InstallPlugin builds the test plugin with go build and installs it into
// dir under each of the given names. The plugin identifies itself by its
// binary name.
func InstallPlugin(t testing.TB, dir string, names ...string) {
	t.Helper()
	if len(names) == 0 {
		return
	}

	_, file, _, _ := runtime.Caller(0)
	src := filepath.Join(filepath.Dir(file), "..", "testdata", "testplugin")
	goBin := filepath.Join(runtime.GOROOT(), "bin", BinaryName("go"))
	first := filepath.Join(dir, BinaryName(names[0]))

	cmd := exec.Command(goBin, "build", "-o", first, ".")
	cmd.Dir = src
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build the test plugin: %v\n%s", err, out)
	}

	bin, err := os.ReadFile(first)
	if err != nil {
		t.Fatalf("failed to read the test plugin: %v", err)
	}
	for _, name := range names[1:] {
		if err := os.WriteFile(filepath.Join(dir, BinaryName(name)), bin, 0o755); err != nil {
			t.Fatalf("failed to install the test plugin: %v", err)
		}
	}
}

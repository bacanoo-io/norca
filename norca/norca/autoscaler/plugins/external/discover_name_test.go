// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package external

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/autoscaler/plugins/external/externaltest"
	"github.com/shoenig/test/must"
)

// The ".exe" extension of a driver or a binary is removed on Windows only, in
// any case, before the lookup and the check of the plugin's name (review
// finding ASP-12).
func TestDriverName(t *testing.T) {
	ci.Parallel(t)

	if runtime.GOOS == "windows" {
		must.Eq(t, "foo", driverName("foo.exe"))
		must.Eq(t, "FOO", driverName("FOO.EXE"))
		must.Eq(t, "foo", driverName("foo.Exe"))
	} else {
		must.Eq(t, "foo.exe", driverName("foo.exe"))
		must.Eq(t, "FOO.EXE", driverName("FOO.EXE"))
	}
	must.Eq(t, "foo", driverName("foo"))
	must.Eq(t, "foo.bin", driverName("foo.bin"))

	dir := t.TempDir()
	must.NoError(t, os.WriteFile(filepath.Join(dir, "UPPER.EXE"), []byte("x"), 0o755))
	names, err := Discover(dir)
	must.NoError(t, err)
	if runtime.GOOS == "windows" {
		must.Eq(t, []string{"UPPER"}, names)
	} else {
		must.Eq(t, []string{"UPPER.EXE"}, names)
	}
}

// On Windows a driver named with its extension starts: the plugin identifies
// itself without it.
func TestLoader_DriverWithExtension(t *testing.T) {
	ci.Parallel(t)
	if runtime.GOOS != "windows" {
		t.Skip("the extension is a part of the name of a file here")
	}
	dir := t.TempDir()
	externaltest.InstallPlugin(t, dir, "test-apm")

	l, _ := newTestLoader(t, dir)
	apm, err := l.APM("my-apm", "test-apm.EXE", nil, nil)
	must.NoError(t, err)
	ms, err := apm.Query("2", testRange())
	must.NoError(t, err)
	must.Eq(t, 2.0, ms[0].Value)
}

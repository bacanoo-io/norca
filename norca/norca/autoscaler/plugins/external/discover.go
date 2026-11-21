// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package external

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Discover returns the names of the plugin binaries in dir: the executable
// regular files, with the ".exe" extension removed on Windows. A missing
// directory is not an error.
func Discover(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read plugin directory %q: %w", dir, err)
	}

	var names []string
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		info, err := os.Stat(path) // follows symlinks
		if err != nil || !info.Mode().IsRegular() || !executable(path, info) {
			continue
		}
		names = append(names, driverName(e.Name()))
	}
	sort.Strings(names)
	return names, nil
}

// driverName returns the name of the plugin of a driver or a binary: on
// Windows without its ".exe" extension, in any case, as a binary is executable
// there; elsewhere the name as it is, since ".exe" is a part of the name of
// a file there (review finding ASP-12: the extension was removed on every
// system and only in lower case, so "foo.exe" looked up a file "foo" on Unix,
// FOO.EXE was listed with its extension, and on Windows a driver "foo.exe"
// was found but refused, its plugin identifying itself as "foo").
func driverName(driver string) string {
	if runtime.GOOS != "windows" {
		return driver
	}
	if ext := filepath.Ext(driver); strings.EqualFold(ext, ".exe") {
		return strings.TrimSuffix(driver, ext)
	}
	return driver
}

// resolve returns the path of the binary of driver in dir.
func resolve(dir, driver string) (string, error) {
	if driver == "" || strings.ContainsAny(driver, `/\`) || driver == "." || driver == ".." {
		return "", fmt.Errorf("invalid plugin driver %q", driver)
	}

	name := driverName(driver)
	candidates := []string{filepath.Join(dir, name)}
	if runtime.GOOS == "windows" {
		candidates = []string{filepath.Join(dir, name+".exe")}
	}
	for _, path := range candidates {
		info, err := os.Stat(path)
		if err == nil && info.Mode().IsRegular() && executable(path, info) {
			return path, nil
		}
	}

	available, _ := Discover(dir)
	return "", fmt.Errorf("plugin binary %q not found in %q (available: %s)",
		name, dir, strings.Join(available, ", "))
}

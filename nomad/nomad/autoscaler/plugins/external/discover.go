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
		names = append(names, strings.TrimSuffix(e.Name(), ".exe"))
	}
	sort.Strings(names)
	return names, nil
}

// resolve returns the path of the binary of driver in dir.
func resolve(dir, driver string) (string, error) {
	if driver == "" || strings.ContainsAny(driver, `/\`) || driver == "." || driver == ".." {
		return "", fmt.Errorf("invalid plugin driver %q", driver)
	}

	name := strings.TrimSuffix(driver, ".exe")
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

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package supplychain

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// validTag is the OCI distribution grammar of a tag.
var validTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

// ValidateTag checks a tag that comes from outside (a bundle index, an OCI
// layout, a registry tag list or the command line) before it becomes part of
// a pack cache path. Besides the OCI tag grammar, which already excludes path
// separators, drive letters and "..", it rejects the names Windows reserves
// for devices and names it would silently shorten.
func ValidateTag(tag string) error {
	if !validTag.MatchString(tag) || !portableName(tag) {
		return fmt.Errorf("invalid tag %q", tag)
	}
	return nil
}

// ValidatePathName checks a name that becomes one element of a pack cache
// path (a pack name or a cache registry name), on every OS: it must not be
// empty, "." or "..", contain a path separator of either kind, a drive or
// stream separator (":"), the reference separator "@", characters Windows
// does not allow in file names or control characters, be absolute, or be a
// name Windows reserves for devices.
func ValidatePathName(kind, name string) error {
	invalid := name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, `/\:@*?"<>|`) ||
		strings.IndexFunc(name, unicode.IsControl) >= 0 ||
		filepath.IsAbs(name) || filepath.VolumeName(name) != "" ||
		!portableName(name)
	if invalid {
		return fmt.Errorf("invalid %s %q", kind, name)
	}
	return nil
}

// windowsReservedNames are the device names Windows reserves in every
// directory, with any extension.
var windowsReservedNames = func() map[string]bool {
	m := map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true}
	for _, d := range []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³"} {
		m["COM"+d] = true
		m["LPT"+d] = true
	}
	return m
}()

// portableName reports whether a file name means the same file on every OS:
// not a reserved device name and without a trailing dot or space, which
// Windows strips.
func portableName(name string) bool {
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return false
	}
	base, _, _ := strings.Cut(name, ".")
	return !windowsReservedNames[strings.ToUpper(strings.TrimRight(base, " "))]
}

// within reports whether path p is root or below it, both cleaned.
func within(root, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

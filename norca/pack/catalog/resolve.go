// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package catalog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/bacanoo-io/norca/norca/pack/internal/pkg/caching"
)

// The pack cache of a server is cacheDir/<registry>/<ref>/<pack>@<ref>. The
// names of a registry, of a ref, of a pack and of a version reach the servers
// in requests; none of them may ever choose a directory outside of the cache
// of its registry.

// ErrPackNotCached is returned by FindPackDir when the cache of the registry
// holds no directory of the pack.
var ErrPackNotCached = errors.New("pack not found in the cache of the registry")

// maxPathSegmentLength is the longest name a file system takes.
const maxPathSegmentLength = 255

// checkPathSegment refuses a name that is not one plain element of a path
// below a directory, on any platform: the servers of a cluster may run on
// both path styles, so "\" and ":" (a Windows separator, drive or stream)
// are refused on Unix too, and what the platform itself does not take as a
// local name (the device names of Windows) is refused where it applies.
func checkPathSegment(what, name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%s is empty", what)
	case len(name) > maxPathSegmentLength:
		return fmt.Errorf("%s is longer than %d bytes", what, maxPathSegmentLength)
	case name == "." || name == "..":
		return fmt.Errorf("%s %q is not a name", what, name)
	}
	for _, r := range name {
		switch {
		case r == '/' || r == '\\' || r == ':':
			return fmt.Errorf("%s %q contains %q: it must be a plain name, not a path", what, name, string(r))
		case r < 0x20 || r == 0x7f:
			return fmt.Errorf("%s %q contains a control character", what, name)
		}
	}
	if !filepath.IsLocal(name) {
		return fmt.Errorf("%s %q is not a plain name", what, name)
	}
	return nil
}

// ValidateRegistryName refuses a registry name that can't be the name of
// the registry's directory in the pack cache.
func ValidateRegistryName(name string) error {
	return checkPathSegment("the registry name", name)
}

// ValidatePackName refuses a pack name of a request that is not a plain
// name: it is compared with the names of the cached packs of a registry and
// must never be a path.
func ValidatePackName(name string) error {
	if err := checkPathSegment("the pack name", name); err != nil {
		return err
	}
	if strings.Contains(name, "@") {
		return fmt.Errorf("the pack name %q contains \"@\": give the version apart from the name", name)
	}
	return nil
}

// ValidatePackVersion refuses a pack version (or ref) of a request that is
// not a plain name. An empty version is the default one.
func ValidatePackVersion(version string) error {
	if version == "" {
		return nil
	}
	return checkPathSegment("the pack version", version)
}

// ValidateRef refuses a default ref of a registry that would not stay in
// the directory of the registry in the pack cache. A git ref may have "/"
// (the cache escapes it); the ref of every other source is the name of a
// directory. No ref starts with "-": it is passed to the tools that fetch
// the source.
func ValidateRef(sourceType, ref string) error {
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("the ref %q starts with \"-\"", ref)
	}
	if sourceType == "git" {
		ref = caching.EscapeRef(ref)
	}
	return checkPathSegment("the ref", ref)
}

// PackLookup names a pack in the cache of a registry.
type PackLookup struct {
	// CacheDir is the pack cache; Registry the name of the registry.
	CacheDir string
	Registry string

	// Pack is the name and Version the version or ref the caller asked for
	// ("" for any).
	Pack    string
	Version string

	// Refs are the refs of the registry to look in after Version, in order.
	Refs []string

	// AnyRef accepts the pack under any ref when it has no directory named
	// after Version: for a version that the catalog of the registry lists
	// from the metadata of a pack fetched by another ref.
	AnyRef bool
}

// FindPackDir returns the directory of a pack in the cache of a registry, or
// ErrPackNotCached.
//
// The names of the request are only ever compared with the names of the
// directories that are in the cache of the registry: no path is built from
// them, so whatever they are, the result is a directory two levels below
// CacheDir/Registry. The cache is read through an os.Root of that directory,
// and a symbolic link is never taken for a ref or a pack directory.
func FindPackDir(q PackLookup) (string, error) {
	if err := ValidateRegistryName(q.Registry); err != nil {
		return "", err
	}
	if err := ValidatePackName(q.Pack); err != nil {
		return "", err
	}
	if err := ValidatePackVersion(q.Version); err != nil {
		return "", err
	}

	regDir := filepath.Join(q.CacheDir, q.Registry)
	root, err := os.OpenRoot(regDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrPackNotCached
		}
		return "", err
	}
	defer root.Close()
	fsys := root.FS()

	// packs maps the ref directories to their pack directories, in the
	// order of their names.
	refDirs, err := cacheDirs(fsys, ".")
	if err != nil {
		return "", err
	}
	packs := make(map[string][]string, len(refDirs))
	for _, ref := range refDirs {
		dirs, err := cacheDirs(fsys, ref)
		if err != nil {
			continue
		}
		packs[ref] = dirs
	}
	found := func(ref, dir string) string {
		return filepath.Join(regDir, ref, dir)
	}

	// The directory named after the asked version, then after the refs of
	// the registry.
	if q.Version != "" {
		for _, ref := range append([]string{q.Version}, q.Refs...) {
			want := q.Pack + "@" + ref
			for _, dir := range packs[ref] {
				if dir == want {
					return found(ref, dir), nil
				}
			}
		}
	}
	// Else the first directory of the pack: of the asked version, or of any
	// when none was asked or the catalog lists the version.
	for _, ref := range refDirs {
		for _, dir := range packs[ref] {
			name, version, ok := strings.Cut(dir, "@")
			if !ok || name != q.Pack {
				continue
			}
			if q.Version == "" || version == q.Version || q.AnyRef {
				return found(ref, dir), nil
			}
		}
	}
	return "", ErrPackNotCached
}

// cacheDirs returns the names of the directories in dir, sorted, without
// the hidden ones (the staging and download directories of the cache) and
// without symbolic links.
func cacheDirs(fsys fs.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

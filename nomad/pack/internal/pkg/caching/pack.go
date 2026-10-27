// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package caching

import (
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/hashicorp/nomad/pack/sdk/pack"
)

// PackConfig represents the common configuration required by all packs. Used primarily
// by the cli package but should
type PackConfig struct {
	Registry   string
	Name       string
	Ref        string
	Path       string
	SourcePath string

	// RefDefaulted is set by Init when no ref was given (Norca). The ref of a
	// registry pack is then its default ref in the cache (see
	// DefaultPackRef), and the default deployment name is the pack name
	// without the ref.
	RefDefaulted bool
}

func (cfg *PackConfig) Init() {
	// Set defaults on pack config
	if cfg.Registry == "" {
		cfg.Registry = DefaultRegistryName
	}

	if cfg.Ref == "" {
		cfg.Ref = DefaultRef
		cfg.RefDefaulted = true
	}

	// If the passed source is a directory path, then set directory based defaults.
	packPath, pathErr := filepath.Abs(cfg.Name)
	if pathErr == nil {
		_, pathErr = os.Stat(packPath)
	}
	if pathErr == nil {
		cfg.initFromDirectory(packPath)
	} else {
		cfg.initFromArgs()
	}
}

func (cfg *PackConfig) initFromDirectory(packPath string) {
	// Keep the original user argument so that we can explain how to manage in output
	cfg.SourcePath = cfg.Name
	if runtime.GOOS == "windows" {
		cfg.Path = strings.ReplaceAll(packPath, "\\", "/")
	} else {
		cfg.Path = packPath
	}
	cfg.Name = path.Base(cfg.Path)
	cfg.Registry = DevRegistryName
	cfg.Ref = DevRef
}

// initFromArgs is a utility function to build a pack path for registry added
// packs. Not for use with file system based packs.
func (cfg *PackConfig) initFromArgs() {
	if cfg.RefDefaulted {
		cfg.Ref = DefaultPackRef(DefaultCachePath(), cfg.Registry, cfg.Name)
	}
	// Escape the pack name so the constructed path matches the directory name
	// stored on disk. See EscapePackName for details.
	escapedName := EscapePackName(cfg.Name)
	// Escape the ref so that slashes in git refs (e.g. "pack-name/v1.0.0") are
	// stored as a single path component rather than creating sub-directories.
	escapedRef := EscapeRef(cfg.Ref)
	cfg.Path = path.Join(DefaultCachePath(), cfg.Registry, escapedRef, escapedName)
	if cfg.Ref != "" {
		cfg.Path = AppendRef(cfg.Path, escapedRef)
	}
}

// DefaultPackRef returns the ref of a cached registry pack that is used when
// no ref is given (Norca): the ref the registry was last added or updated
// with if the pack is cached at it, otherwise "latest" if the pack is cached
// at it, otherwise the only ref the pack is cached at. If none of these
// applies, it returns DefaultRef.
func DefaultPackRef(cachePath, registryName, packName string) string {
	registryPath := path.Join(cachePath, registryName)
	cachedAt := func(ref string) bool {
		escapedRef := EscapeRef(ref)
		info, err := os.Stat(path.Join(registryPath, escapedRef, AppendRef(EscapePackName(packName), escapedRef)))
		return err == nil && info.IsDir()
	}

	if b, err := os.ReadFile(path.Join(registryPath, DefaultRefFile)); err == nil {
		if ref := strings.TrimSpace(string(b)); ref != "" && cachedAt(ref) {
			return ref
		}
	}
	if cachedAt(DefaultRef) {
		return DefaultRef
	}

	entries, err := os.ReadDir(registryPath)
	if err != nil {
		return DefaultRef
	}
	var refs []string
	for _, e := range entries {
		if ref := UnescapeRef(e.Name()); e.IsDir() && cachedAt(ref) {
			refs = append(refs, ref)
		}
	}
	if len(refs) == 1 {
		return refs[0]
	}
	return DefaultRef
}

// Pack wraps a pack.Pack add adds the local cache ref. Useful for
// showing the registry in the global cache differentiated from the pack metadata.
type Pack struct {
	Ref string
	*pack.Pack
}

func invalidPackDefinition(provider cacheOperationProvider) *Pack {
	return &Pack{
		Ref: provider.AtRef(),
		Pack: &pack.Pack{
			Metadata: &pack.Metadata{
				Pack: &pack.MetadataPack{
					Name:        provider.ForPackName(),
					Description: "",
					Version:     "Invalid pack definition",
				},
			},
		},
	}
}

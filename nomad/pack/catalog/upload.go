// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package catalog

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hashicorp/nomad/pack/internal/pkg/bundle"
	"github.com/hashicorp/nomad/pack/internal/pkg/loader"
)

const (
	// MaxUploadSize is the largest pack archive that can be uploaded.
	MaxUploadSize = 8 << 20

	// UploadFormatBundle and UploadFormatArchive are the upload formats:
	// a bundle of norca pack bundle create, or a tar or tar.gz of pack
	// directories.
	UploadFormatBundle  = "bundle"
	UploadFormatArchive = "archive"

	maxUploadExtracted = 64 << 20
	maxUploadFiles     = 4096
)

var validUploadVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

// UploadFormat returns the format of an uploaded archive.
func UploadFormat(data []byte) (string, error) {
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		return UploadFormatArchive, nil
	}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return UploadFormatArchive, nil
		}
		if err != nil {
			return "", errors.New("the upload is neither a pack bundle nor a tar or tar.gz archive")
		}
		if path.Clean(h.Name) == bundle.IndexFileName {
			return UploadFormatBundle, nil
		}
	}
}

// ImportUpload imports an uploaded pack archive into cacheDir/<registry>:
// the packs of a bundle, verified with the trusted keys, or the pack
// directories of a tar or tar.gz archive, under <version>/<pack>@<version>.
// Archives carry no signatures, so a registry with trusted keys takes
// bundles only.
func ImportUpload(cacheDir, registryName string, data []byte, trustedKeys []string) (string, error) {
	if len(data) > MaxUploadSize {
		return "", fmt.Errorf("the upload is larger than %d MiB", MaxUploadSize>>20)
	}
	format, err := UploadFormat(data)
	if err != nil {
		return "", err
	}
	switch format {
	case UploadFormatBundle:
		f, err := os.CreateTemp("", "norca-upload-*.tar")
		if err != nil {
			return format, err
		}
		defer os.Remove(f.Name())
		if _, err := f.Write(data); err != nil {
			f.Close()
			return format, err
		}
		f.Close()
		b, err := bundle.Open(f.Name())
		if err != nil {
			return format, err
		}
		defer b.Close()
		policy, cleanup := policyForKeys(trustedKeys)
		defer cleanup()
		if _, err := b.ImportToCache(context.Background(), cacheDir, registryName, policy); err != nil {
			return format, fmt.Errorf("failed to import the bundle: %w", err)
		}
		return format, nil
	default:
		if len(trustedKeys) > 0 {
			return format, errors.New("the registry has trusted keys: upload a signed bundle (norca pack bundle create --sign-key)")
		}
		return format, importArchive(cacheDir, registryName, data)
	}
}

// importArchive extracts a tar or tar.gz of pack directories (or of one
// pack at its root) and copies each pack into the cache.
func importArchive(cacheDir, registryName string, data []byte) error {
	tmp, err := os.MkdirTemp("", "norca-upload-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := extractArchive(data, tmp); err != nil {
		return err
	}

	var roots []string
	if fileExists(filepath.Join(tmp, "metadata.hcl")) {
		roots = []string{tmp}
	} else {
		entries, err := os.ReadDir(tmp)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.IsDir() && fileExists(filepath.Join(tmp, e.Name(), "metadata.hcl")) {
				roots = append(roots, filepath.Join(tmp, e.Name()))
			}
		}
	}
	if len(roots) == 0 {
		return errors.New("the archive has no pack: no metadata.hcl at its root or in a directory of its root")
	}
	for _, root := range roots {
		p, err := loader.Load(root)
		if err != nil {
			return fmt.Errorf("pack %s: %w", filepath.Base(root), err)
		}
		if p.Metadata == nil || p.Metadata.Pack == nil || p.Metadata.Pack.Name == "" {
			return fmt.Errorf("pack %s: metadata.hcl names no pack", filepath.Base(root))
		}
		name, version := p.Metadata.Pack.Name, p.Metadata.Pack.Version
		if !validUploadVersion.MatchString(name) || !validUploadVersion.MatchString(version) {
			return fmt.Errorf("pack %q: an uploaded pack needs a name and a version of letters, digits, '.', '_', '+' and '-' (got version %q)", name, version)
		}
		dst := filepath.Join(cacheDir, registryName, version, name+"@"+version)
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		if err := copyPackDirectory(root, dst); err != nil {
			return err
		}
	}
	return nil
}

// extractArchive extracts regular files and directories of a tar or tar.gz
// into dir. Absolute names, names leaving dir, links and devices are
// refused, and the size and number of files are limited.
func extractArchive(data []byte, dir string) error {
	var r io.Reader = bytes.NewReader(data)
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return fmt.Errorf("invalid gzip data: %w", err)
		}
		defer gz.Close()
		r = gz
	}
	tr := tar.NewReader(r)
	var total int64
	files := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("invalid tar archive: %w", err)
		}
		name := path.Clean(strings.ReplaceAll(h.Name, "\\", "/"))
		if name == "." || name == "pax_global_header" {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, ":") {
			return fmt.Errorf("the archive entry %q leaves the archive", h.Name)
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			files++
			if files > maxUploadFiles {
				return fmt.Errorf("the archive has more than %d files", maxUploadFiles)
			}
			total += h.Size
			if h.Size < 0 || total > maxUploadExtracted {
				return fmt.Errorf("the archive extracts to more than %d MiB", maxUploadExtracted>>20)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, io.LimitReader(tr, h.Size))
			f.Close()
			if err != nil {
				return err
			}
		case tar.TypeXHeader, tar.TypeXGlobalHeader:
		default:
			return fmt.Errorf("the archive entry %q is not a file or directory", h.Name)
		}
	}
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

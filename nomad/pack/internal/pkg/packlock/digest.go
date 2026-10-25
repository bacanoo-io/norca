// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package packlock computes reproducible content digests of pack directories
// and reads, writes and verifies the pack.lock file that records the digests
// of a pack and of its vendored dependencies.
package packlock

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// FileName is the name of the lock file in the root of a pack.
	FileName = "pack.lock"

	// OriginFileName is the name of the file that records where a pack in the
	// local cache was downloaded from. It is written by the client and never
	// part of a pack's content.
	OriginFileName = ".norca-origin.json"

	// DepsDir is the directory of a pack that holds vendored dependencies.
	DepsDir = "deps"

	// digestPrefix is the algorithm prefix of every digest computed here.
	digestPrefix = "sha256:"
)

// File is one regular file of a pack directory, addressed by its slash
// separated path relative to the pack root.
type File struct {
	Path string
	Abs  string
	Size int64
}

// Filter decides whether a relative, slash separated path is part of the
// content. Directories that are rejected are skipped entirely.
type Filter func(rel string, isDir bool) bool

// ContentFilter selects the full content of a pack as it is published: every
// file except version control data and the client's origin record.
func ContentFilter(rel string, isDir bool) bool {
	base := rel
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		base = rel[i+1:]
	}
	if isDir {
		return base != ".git"
	}
	return rel != OriginFileName
}

// OwnContentFilter selects the content that belongs to the pack itself: the
// full content without vendored dependencies and without the lock file.
func OwnContentFilter(rel string, isDir bool) bool {
	if !ContentFilter(rel, isDir) {
		return false
	}
	if isDir {
		return rel != DepsDir
	}
	return rel != FileName
}

// Files lists the regular files of root accepted by filter, sorted by path.
// Symbolic links and other special files are rejected, since packs are copied
// between machines and must not point outside of themselves.
func Files(root string, filter Filter) ([]File, error) {
	var files []File
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if filter != nil && !filter(rel, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if filter != nil && !filter(rel, false) {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: only regular files are supported in packs", rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files = append(files, File{Path: rel, Abs: p, Size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// TreeDigest returns the content digest of the files of root accepted by
// filter. The digest only depends on the relative paths and the contents of
// the files, not on timestamps, permissions or the operating system: it is the
// SHA-256 of the sorted lines "<sha256 of file>  <path>\n".
func TreeDigest(root string, filter Filter) (string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", root)
	}

	files, err := Files(root, filter)
	if err != nil {
		return "", err
	}

	sum := sha256.New()
	for _, f := range files {
		fileSum, err := fileDigest(f.Abs)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(sum, "%s  %s\n", fileSum, f.Path)
	}
	return digestPrefix + hex.EncodeToString(sum.Sum(nil)), nil
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

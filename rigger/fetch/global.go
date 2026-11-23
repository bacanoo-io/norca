// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	git "github.com/go-git/go-git/v5"

	"github.com/bacanoo-io/norca/rigger/index"
	"github.com/bacanoo-io/norca/rigger/source"
)

// DefaultGlobalRegistry is the address of the global registry (design
// 7.3), the default registry of Rigger.
const DefaultGlobalRegistry = "github.com/bacanoo-io/norca-global-registry"

// SetGlobalRegistry sets the address of the global registry: a git address,
// or the absolute path of a local directory, read as it is (a checkout, a registry of the same
// shape an organization runs for itself).
func (f *Fetcher) SetGlobalRegistry(address string) {
	f.indexMu.Lock()
	defer f.indexMu.Unlock()

	f.global, f.index = address, nil
}

// globalIndex is the files of the global registry.
type globalIndex struct {
	fsys       fs.FS
	advisories *index.Advisories
}

// ErrBlocked is the error of a version the global registry blocks.
var ErrBlocked = errors.New("blocked by the global registry")

// readIndex returns the files of the global registry: a directory as it is,
// else the tree of the default branch of its mirror in the cache, fetched
// once per run. When the repository can't be reached, the mirror of an
// earlier run serves: entries are never changed, only advisories may be
// missing.
func (f *Fetcher) readIndex(ctx context.Context) (*globalIndex, error) {
	f.indexMu.Lock()
	defer f.indexMu.Unlock()

	if f.index != nil {
		return f.index, nil
	}

	fsys, err := f.indexFS(ctx)
	if err != nil {
		return nil, fmt.Errorf("the global registry %s: %w", source.Redact(f.global), err)
	}

	adv, err := index.ReadAdvisories(fsys)
	if err != nil {
		return nil, fmt.Errorf("the global registry %s: %w", source.Redact(f.global), err)
	}

	f.index = &globalIndex{fsys: fsys, advisories: adv}

	return f.index, nil
}

func (f *Fetcher) indexFS(ctx context.Context) (fs.FS, error) {
	if info, err := os.Stat(f.global); err == nil && info.IsDir() && filepath.IsAbs(f.global) {
		return os.DirFS(f.global), nil
	}

	r, err := f.openRepo(ctx, f.global)
	if err != nil {
		// Without a network the mirror of an earlier run serves.
		cached, openErr := git.PlainOpen(f.mirrorDir(f.global))
		if openErr != nil {
			return nil, err
		}

		r = cached
	}

	commit, err := headCommit(r)
	if err != nil {
		return nil, err
	}

	tree := filepath.Join(f.cache, "index", commit)
	if _, err := os.Stat(tree); err != nil {
		if err := extract(r, commit, tree); err != nil {
			return nil, err
		}
	}

	return os.DirFS(tree), nil
}

// entryKind is the kind of the entry of a request.
func entryKind(req source.Request) index.Kind {
	if req.Stack {
		return index.KindStack
	}

	return index.KindModule
}

// globalVersions lists the versions the entry of a module lists, but the
// blocked ones.
func (f *Fetcher) globalVersions(ctx context.Context, req source.Request) ([]source.Available, error) {
	idx, err := f.readIndex(ctx)
	if err != nil {
		return nil, err
	}

	kind := entryKind(req)

	entry, err := index.ReadEntry(idx.fsys, kind, req.Namespace, req.Name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("the global registry lists no %s %s/%s: %w", kind, req.Namespace, req.Name, err)
	}

	if err != nil {
		return nil, err
	}

	var out []source.Available

	for _, v := range entry.Versions {
		if _, blocked := idx.advisories.Blocked(kind, req.Namespace, req.Name, v.Version); blocked {
			continue
		}

		out = append(out, source.Available{Version: v.Version, Ref: v.Tag, Commit: v.Commit})
	}

	return out, nil
}

// globalFetch fetches a listed version from the repository of its author,
// at the commit the entry records, with the listing the engine checks it
// by. When the global registry can't be read at all, a version the lock
// file records is fetched as the git source it is (from a tree in the
// cache or a bundle), without the listing.
func (f *Fetcher) globalFetch(ctx context.Context, req source.Request) (source.Fetched, error) {
	idx, err := f.readIndex(ctx)
	if err != nil {
		if req.Commit != "" && req.Repository != "" {
			return f.gitFetch(ctx, source.Request{
				Source: req.Source, Kind: source.KindGit, Repository: req.Repository, Path: req.Path,
				Version: req.Version, Ref: req.Ref, Commit: req.Commit, Digest: req.Digest,
			})
		}

		return source.Fetched{}, err
	}

	kind := entryKind(req)

	entry, err := index.ReadEntry(idx.fsys, kind, req.Namespace, req.Name)
	if err != nil {
		return source.Fetched{}, err
	}

	v := entry.Version(req.Version)
	if v == nil {
		return source.Fetched{}, fmt.Errorf("the global registry lists no version %s of %s: %w", req.Version, req.Source, fs.ErrNotExist)
	}

	if adv, blocked := idx.advisories.Blocked(kind, req.Namespace, req.Name, v.Version); blocked {
		return source.Fetched{}, fmt.Errorf("%s %s: %w: %s (%s)", req.Source, v.Version, ErrBlocked, adv.Reason, adv.Decision)
	}

	if req.Commit != "" && req.Commit != v.Commit {
		return source.Fetched{}, fmt.Errorf("%s %s: the global registry lists the commit %s, the lock file %s", req.Source, v.Version, v.Commit, req.Commit)
	}

	fetched, err := f.gitFetch(ctx, source.Request{
		Source: req.Source, Kind: source.KindGit, Repository: v.Repository, Path: v.Path,
		Version: v.Version, Ref: v.Tag, Commit: v.Commit, Digest: v.Digest,
	})
	if err != nil {
		return source.Fetched{}, err
	}

	keys, err := index.ReadKeys(idx.fsys, req.Namespace)
	if err != nil {
		return source.Fetched{}, err
	}

	listing := &source.Listing{Name: entry.Name, Digest: v.Digest, Keys: keys}

	for _, s := range v.Signatures {
		listing.Signatures = append(listing.Signatures, source.ListedSignature{KeyID: s.KeyID, Signature: s.Signature})
	}

	for _, adv := range idx.advisories.For(kind, req.Namespace, req.Name, v.Version) {
		listing.Warnings = append(listing.Warnings, adv.Reason)
	}

	fetched.Origin.Repository = v.Repository
	fetched.Listing = listing

	return fetched, nil
}

// Tag is a tag of a repository and the commit it points to.
type Tag struct {
	Name   string
	Commit string
}

// Tags lists the tags of a git repository and their commits, from its
// mirror in the cache (or in place, for a repository on the local file
// system).
func (f *Fetcher) Tags(ctx context.Context, repository string) ([]Tag, error) {
	r, err := f.openRepo(ctx, repository)
	if err != nil {
		return nil, err
	}

	return tagCommits(r, "")
}

// Tree returns the files of a git repository at a tag, and the commit of
// the tag.
func (f *Fetcher) Tree(ctx context.Context, repository, tag string) (fs.FS, string, error) {
	fetched, err := f.gitFetch(ctx, source.Request{Kind: source.KindGit, Source: repository, Repository: repository, Path: ".", Ref: tag})
	if err != nil {
		return nil, "", err
	}

	return fetched.FS, fetched.Origin.Commit, nil
}

// WithGlobalRegistry returns a fetcher of the cache, locks and credentials
// of f that reads the global registry at address. A host that serves
// several requests gives each its own: the index one request reads is not
// moved by another, and each is a run of its own, which fetches a mirror
// once.
func (f *Fetcher) WithGlobalRegistry(address string) *Fetcher {
	return &Fetcher{shared: f.shared, global: address, updated: map[string]bool{}}
}

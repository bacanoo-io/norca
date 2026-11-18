// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package indexer keeps the files of the global registry (design 7.3): it
// generates the entries of a namespace from the tags of its repositories,
// and checks entries against their sources, the way the workflows of the
// registry repository run them.
package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	rig "github.com/bacanoo-io/norca-rig"
	"github.com/bacanoo-io/norca-rig/cli/internal/fetch"
	"github.com/bacanoo-io/norca-rig/index"
	"github.com/bacanoo-io/norca-rig/sign"
	"github.com/bacanoo-io/norca-rig/version"
)

// Indexer works on a checkout of a registry of the shape of the global
// registry.
type Indexer struct {
	// Dir is the checkout of the registry.
	Dir string
	// Engine loads and validates modules; it renders their examples with
	// the decoders of the host.
	Engine  *rig.Engine
	Fetcher *fetch.Fetcher
	// Replace reads a repository from another place (a clone), keyed by
	// its URL; the entries still name the URL.
	Replace map[string]string
	// Today is the day a version is listed on, YYYY-MM-DD.
	Today string
}

// Outcome is what became of one tag.
type Outcome struct {
	Namespace string
	Name      string
	Version   string
	Tag       string
	// Listed is set for a version added to its entry.
	Listed bool
	// Problem is why the tag is not listed, or what is wrong with a listed
	// version.
	Problem string
}

func (o Outcome) String() string {
	if o.Listed {
		return fmt.Sprintf("listed %s/%s %s (%s)", o.Namespace, o.Name, o.Version, o.Tag)
	}

	return fmt.Sprintf("%s/%s %s (%s): %s", o.Namespace, o.Name, o.Version, o.Tag, o.Problem)
}

// fsys is the files of the registry.
func (ix *Indexer) fsys() fs.FS {
	return os.DirFS(ix.Dir)
}

// location is where a repository is read from.
func (ix *Indexer) location(url string) string {
	if p, ok := ix.Replace[url]; ok {
		return p
	}

	return url
}

// Namespaces returns the names of the namespaces of the registry.
func (ix *Indexer) Namespaces() ([]string, error) {
	entries, err := fs.ReadDir(ix.fsys(), "namespaces")
	if err != nil {
		return nil, err
	}

	var names []string

	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".json"); ok && !e.IsDir() {
			names = append(names, name)
		}
	}

	return names, nil
}

// keys reads and parses the keys of a namespace.
func (ix *Indexer) keys(namespace string) ([]sign.PublicKey, error) {
	texts, err := index.ReadKeys(ix.fsys(), namespace)
	if err != nil {
		return nil, err
	}

	keys := make([]sign.PublicKey, 0, len(texts))

	for _, text := range texts {
		k, err := sign.ParsePublicKey(text)
		if err != nil {
			return nil, fmt.Errorf("a key of the namespace %s: %w", namespace, err)
		}

		keys = append(keys, k)
	}

	return keys, nil
}

// versionTag is a tag of a module version of a repository.
type versionTag struct {
	name, version, dir string
	kind               index.Kind
}

// parseTag reads a tag of a repository of a layout: <name>/v<version> of a
// module in modules/<name> or a stack in stacks/<name> of a registry
// repository, v<version> of a repository of one module.
func parseTag(repo index.Repository, tag string) (versionTag, bool) {
	var vt versionTag

	switch repo.Layout {
	case index.LayoutRegistry:
		name, v, ok := strings.Cut(tag, "/v")
		if !ok || !index.ValidName(name) {
			return vt, false
		}

		vt = versionTag{name: name, version: v, dir: "modules/" + name, kind: index.KindModule}
	case index.LayoutModule:
		v, ok := strings.CutPrefix(tag, "v")
		if !ok {
			return vt, false
		}

		vt = versionTag{name: repositoryName(repo.URL), version: v, dir: ".", kind: index.KindModule}
	default:
		return vt, false
	}

	if _, err := version.Parse(vt.version); err != nil {
		return vt, false
	}

	return vt, true
}

// expectedTag is the tag and directory of a version of a module of a
// repository.
func expectedTag(repo index.Repository, name, v string) (tag, dir string) {
	if repo.Layout == index.LayoutModule {
		return "v" + v, "."
	}

	return name + "/v" + v, "modules/" + name
}

// repositoryName is the last element of the path of a repository, without
// .git.
func repositoryName(url string) string {
	return strings.TrimSuffix(path.Base(url), ".git")
}

var (
	errNotSigned = errors.New("not signed by a key of the namespace")
	errStack     = errors.New("stacks of a registry are not listed yet")
)

// inspect reads a version from its source at a tag, validates it, and
// returns what the index lists of it: the signatures that verify with a
// key of the namespace only, errNotSigned with the rest when there are
// none.
func (ix *Indexer) inspect(ctx context.Context, keys []sign.PublicKey, url string, vt versionTag, tag string) (index.Version, error) {
	fsys, commit, err := ix.Fetcher.Tree(ctx, ix.location(url), tag)
	if err != nil {
		return index.Version{}, err
	}

	if _, err := fs.Stat(fsys, path.Join(vt.dir, "metadata.hcl")); err != nil {
		if _, stackErr := fs.Stat(fsys, path.Join("stacks", vt.name, "metadata.hcl")); stackErr == nil {
			return index.Version{}, errStack
		}

		return index.Version{}, fmt.Errorf("no module in %s at the tag", vt.dir)
	}

	m, diags := ix.Engine.LoadModuleAt(ctx, fsys, vt.dir)
	if diags.HasErrors() {
		return index.Version{}, diags
	}

	md := m.Metadata()
	if md.Name != vt.name || md.Version != vt.version {
		return index.Version{}, fmt.Errorf("the metadata says %s %s", md.Name, md.Version)
	}

	if diags := ix.Engine.ValidateModule(ctx, m); diags.HasErrors() {
		return index.Version{}, diags
	}

	digest, err := m.Digest()
	if err != nil {
		return index.Version{}, err
	}

	v := index.Version{
		Version: vt.version, Repository: url, Path: vt.dir, Tag: tag, Commit: commit, Digest: digest,
		Requires: requires(md.Requires.Norca, md.Requires.Features, md.Requires.Modules), Listed: ix.Today,
	}

	// A version without a signature of the namespace is returned with
	// errNotSigned: Check verifies the signatures an entry lists by
	// themselves.
	sigs, signErr := signatures(fsys, vt.dir, digest, md.Name, md.Version, keys)
	if signErr != nil && !errors.Is(signErr, errNotSigned) {
		return index.Version{}, signErr
	}

	v.Signatures = sigs

	if md.Icon != "" {
		data, err := fs.ReadFile(fsys, path.Join(vt.dir, md.Icon))
		if err != nil {
			return index.Version{}, fmt.Errorf("the icon: %w", err)
		}

		sum := sha256.Sum256(data)
		v.Icon = "sha256:" + hex.EncodeToString(sum[:])
	}

	return v, signErr
}

// signatures returns the signatures of a module that a key of the
// namespace makes of the statement of the version, by key.
func signatures(fsys fs.FS, dir, digest, name, v string, keys []sign.PublicKey) ([]index.Signature, error) {
	sigs, err := sign.ReadSignatures(fsys, dir)
	if err != nil {
		return nil, err
	}

	statement, err := sign.Statement(digest, name, v)
	if err != nil {
		return nil, err
	}

	var out []index.Signature

	for _, s := range sigs {
		if s.Payload != statement {
			continue
		}

		for _, id := range sign.Verify(digest, []sign.Signature{s}, keys) {
			if !slices.ContainsFunc(out, func(o index.Signature) bool { return o.KeyID == id }) {
				out = append(out, index.Signature{KeyID: id, Signature: s.Signature})
			}
		}
	}

	if len(out) == 0 {
		return nil, errNotSigned
	}

	slices.SortFunc(out, func(a, b index.Signature) int { return strings.Compare(a.KeyID, b.KeyID) })

	return out, nil
}

// readEntry reads an entry of the registry, nil when there is none.
func (ix *Indexer) readEntry(kind index.Kind, namespace, name string) (*index.Entry, error) {
	e, err := index.ReadEntry(ix.fsys(), kind, namespace, name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil //nolint:nilnil // no entry yet
	}

	return e, err
}

// writeEntry writes an entry, its versions in ascending order.
func (ix *Indexer) writeEntry(kind index.Kind, e *index.Entry) error {
	slices.SortFunc(e.Versions, func(a, b index.Version) int {
		va, _ := version.Parse(a.Version)
		vb, _ := version.Parse(b.Version)

		return version.Compare(va, vb)
	})

	if err := e.Validate(kind); err != nil {
		return err
	}

	data, err := index.Marshal(e)
	if err != nil {
		return err
	}

	p := filepath.Join(ix.Dir, filepath.FromSlash(index.EntryPath(kind, e.Namespace, e.Name)))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}

	return os.WriteFile(p, data, 0o644) //nolint:gosec // G306: the files of a public registry
}

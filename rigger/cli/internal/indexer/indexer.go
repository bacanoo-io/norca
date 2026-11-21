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

	rig "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/cli/internal/fetch"
	"github.com/bacanoo-io/norca/rigger/index"
	"github.com/bacanoo-io/norca/rigger/module"
	"github.com/bacanoo-io/norca/rigger/sign"
	"github.com/bacanoo-io/norca/rigger/source"
	"github.com/bacanoo-io/norca/rigger/version"
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
	// Listed is set for a version added to its entry, of the kind.
	Listed bool
	Kind   index.Kind
	// Problem is why the tag is not listed, or what is wrong with a listed
	// version.
	Problem string
}

func (o Outcome) String() string {
	if o.Listed {
		return fmt.Sprintf("listed %s %s/%s %s (%s)", o.Kind, o.Namespace, o.Name, o.Version, o.Tag)
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

// versionTag is a tag of a version of a repository: of a module or a
// stack, which the tree of the tag tells.
type versionTag struct {
	name, version string
	layout        string
}

// parseTag reads a tag of a repository of a layout: <name>/v<version> of a
// module in modules/<name> or a stack in stacks/<name> of a registry
// repository, v<version> of a repository of one module or stack.
func parseTag(repo index.Repository, tag string) (versionTag, bool) {
	vt := versionTag{layout: repo.Layout}

	switch repo.Layout {
	case index.LayoutRegistry:
		name, v, ok := strings.Cut(tag, "/v")
		if !ok || !index.ValidName(name) {
			return vt, false
		}

		vt.name, vt.version = name, v
	case index.LayoutModule:
		v, ok := strings.CutPrefix(tag, "v")
		if !ok {
			return vt, false
		}

		vt.name, vt.version = repositoryName(repo.URL), v
	default:
		return vt, false
	}

	if _, err := version.Parse(vt.version); err != nil {
		return vt, false
	}

	return vt, true
}

// dir is the directory of a module or stack of the tag.
func (vt versionTag) dir(kind index.Kind) string {
	switch {
	case vt.layout == index.LayoutModule:
		return "."
	case kind == index.KindStack:
		return "stacks/" + vt.name
	default:
		return "modules/" + vt.name
	}
}

// tag is the tag of the version.
func (vt versionTag) tag() string {
	if vt.layout == index.LayoutModule {
		return "v" + vt.version
	}

	return vt.name + "/v" + vt.version
}

// repositoryName is the last element of the path of a repository, without
// .git.
func repositoryName(url string) string {
	return strings.TrimSuffix(path.Base(url), ".git")
}

var (
	errNotSigned = errors.New("not signed by a key of the namespace")
	errNoSource  = errors.New("neither a module nor a stack at the tag")
	errBoth      = errors.New("both a module and a stack of the name; a tag names one")
)

// tree is the files of a tag and what they hold.
type tree struct {
	fsys   fs.FS
	commit string
	kind   index.Kind
	dir    string
}

// readTree fetches the files of a tag and tells whether it holds a module
// or a stack: metadata.hcl without stack.hcl is a module, stack.hcl a stack.
func (ix *Indexer) readTree(ctx context.Context, url string, vt versionTag) (tree, error) {
	fsys, commit, err := ix.Fetcher.Tree(ctx, ix.location(url), vt.tag())
	if err != nil {
		return tree{}, err
	}

	t := tree{fsys: fsys, commit: commit}

	isModule := func(dir string) bool {
		_, metaErr := fs.Stat(fsys, path.Join(dir, "metadata.hcl"))
		_, stackErr := fs.Stat(fsys, path.Join(dir, "stack.hcl"))

		return metaErr == nil && stackErr != nil
	}
	isStack := func(dir string) bool {
		_, err := fs.Stat(fsys, path.Join(dir, "stack.hcl"))

		return err == nil
	}

	module, stack := isModule(vt.dir(index.KindModule)), isStack(vt.dir(index.KindStack))

	switch {
	case module && stack:
		return t, errBoth
	case module:
		t.kind = index.KindModule
	case stack:
		t.kind = index.KindStack
	default:
		return t, errNoSource
	}

	t.dir = vt.dir(t.kind)

	return t, nil
}

// inspect reads a version from the tree of its tag, validates it, and
// returns what the index lists of it: the signatures that verify with a
// key of the namespace only, errNotSigned with the rest when there are
// none.
func (ix *Indexer) inspect(ctx context.Context, keys []sign.PublicKey, url string, vt versionTag, t tree) (index.Version, error) {
	var (
		md     module.Metadata
		digest string
		pins   []index.LockedModule
		err    error
	)

	if t.kind == index.KindStack {
		md, digest, pins, err = ix.inspectStack(ctx, t)
	} else {
		md, digest, err = ix.inspectModule(ctx, t)
	}

	if err != nil {
		return index.Version{}, err
	}

	if md.Name != vt.name || md.Version != vt.version {
		return index.Version{}, fmt.Errorf("the metadata says %s %s", md.Name, md.Version)
	}

	v := index.Version{
		Version: vt.version, Repository: url, Path: t.dir, Tag: vt.tag(), Commit: t.commit, Digest: digest,
		Requires: requires(md.Requires.Norca, md.Requires.Features, md.Requires.Modules), Modules: pins, Listed: ix.Today,
	}

	// A version without a signature of the namespace is returned with
	// errNotSigned: Check verifies the signatures an entry lists by
	// themselves.
	sigs, signErr := signatures(t.fsys, t.dir, digest, md.Name, md.Version, keys)
	if signErr != nil && !errors.Is(signErr, errNotSigned) {
		return index.Version{}, signErr
	}

	v.Signatures = sigs

	if md.Icon != "" {
		data, err := fs.ReadFile(t.fsys, path.Join(t.dir, md.Icon))
		if err != nil {
			return index.Version{}, fmt.Errorf("the icon: %w", err)
		}

		sum := sha256.Sum256(data)
		v.Icon = "sha256:" + hex.EncodeToString(sum[:])
	}

	return v, signErr
}

// inspectModule loads and validates the module of a tree.
func (ix *Indexer) inspectModule(ctx context.Context, t tree) (module.Metadata, string, error) {
	m, diags := ix.Engine.LoadModuleAt(ctx, t.fsys, t.dir)
	if diags.HasErrors() {
		return module.Metadata{}, "", diags
	}

	if diags := ix.Engine.ValidateModule(ctx, m); diags.HasErrors() {
		return module.Metadata{}, "", diags
	}

	digest, err := m.Digest()

	return m.Metadata(), digest, err
}

// ErrOutside is the error of a stack that uses a module outside the
// registry.
var ErrOutside = errors.New("a stack of the registry uses only modules of the registry, its own and built-in ones")

// inspectStack loads a published stack with the modules its lock pins,
// from the registry itself, and validates it with each of its value files.
func (ix *Indexer) inspectStack(ctx context.Context, t tree) (module.Metadata, string, []index.LockedModule, error) {
	s, diags := ix.Engine.LoadStack(ctx, t.fsys, t.dir)
	if diags.HasErrors() {
		return module.Metadata{}, "", nil, diags
	}

	md := s.Metadata()
	if md == nil {
		return module.Metadata{}, "", nil, errors.New("the stack has no metadata.hcl") //nolint:err113 // one message
	}

	var pins []index.LockedModule

	for _, m := range s.LockedModules() {
		if m.Kind != source.KindGlobal && m.Kind != source.KindBuiltin {
			return module.Metadata{}, "", nil, fmt.Errorf("%s: %w", m.Source, ErrOutside)
		}

		pins = append(pins, index.LockedModule{Source: m.Source, Version: m.Version, Digest: m.Digest})
	}

	for _, p := range s.ValueFiles() {
		src, err := s.ReadValueFile(p)
		if err != nil {
			return module.Metadata{}, "", nil, err
		}

		vals, diags := rig.ParseValues(p, src)
		if !diags.HasErrors() {
			diags = append(diags, ix.Engine.ValidateStack(ctx, s, vals)...)
		}

		if diags.HasErrors() {
			return module.Metadata{}, "", nil, diags
		}
	}

	if len(s.ValueFiles()) == 0 {
		if diags := ix.Engine.ValidateStack(ctx, s, nil); diags.HasErrors() {
			return module.Metadata{}, "", nil, diags
		}
	}

	digest, err := s.Digest()

	return *md, digest, pins, err
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

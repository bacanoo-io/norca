// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"
	"sync"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/rigger/fetch"
	"github.com/bacanoo-io/norca/rigger/index"
	"github.com/bacanoo-io/norca/rigger/source"
)

// What the servers fetch from. A request names addresses, and a server
// reads none of them because it was named: it fetches
//
//   - a module built into it (builtin/<name>);
//   - what the global registry lists at the commit of its last sync, from a
//     repository that is no path on the servers;
//   - a git repository registered as a registry, the versions tagged in
//     the history of the commit of its last sync, under the keys of each
//     registry of that repository;
//   - a tree of a bundle imported into the cluster, for exactly the
//     repository, commit and digest a lock file records. A release with
//     such a module, which no index of the servers vouches for, is not
//     rendered by the server.
//
// All but the first need read on the kind of registries, and a private
// repository read on its Variable (rigger_access.go). A directory, an OCI
// registry and any other repository are refused. The modules inside a
// stack that was sent (./modules/x) are not fetched at all.

// riggerSources is the fetcher of one request on a server.
type riggerSources struct {
	registries *riggerRegistries
	// fetcher shares the cache of the server and has the index of the
	// global registry of this request.
	fetcher *fetch.Fetcher
	// aclObj is nil when ACLs are off.
	aclObj *acl.ACL

	// repos are the git registries by their repository.
	repos map[string][]*RiggerRegistry
	// global is the global registry with a good sync, nil without one; its
	// index is read when something of it is asked for (readGlobal).
	global *RiggerRegistry
	// globalDir is the index of the global registry at the commit of its
	// last sync; empty when the request has none.
	globalDir string
	// bundled are the modules of the bundles imported into the cluster.
	bundled []riggerBundledModule

	mu sync.Mutex
	// refused is the first refusal, for the code of the answer: the engine
	// passes on only the text of an error of its fetcher.
	refused error
	// unindexed is set once something came from a bundle alone.
	unindexed bool
}

var _ source.Fetcher = (*riggerSources)(nil)

// riggerBundledModule is a module of an imported bundle: what allows it as
// a source, and where it is in the tree.
type riggerBundledModule struct {
	repository, commit, digest, dir string
}

// newRiggerSources returns the sources of a request of the caller's rights.
func (s *Server) newRiggerSources(args structs.RequestWithIdentity) (*riggerSources, error) {
	aclObj, err := s.ResolveACL(args)
	if err != nil {
		return nil, err
	}

	r, err := s.riggerRegistriesOf()
	if err != nil {
		return nil, err
	}

	fetcher, err := s.riggerRequestFetcher()
	if err != nil {
		return nil, err
	}

	src := &riggerSources{registries: r, fetcher: fetcher, aclObj: aclObj, repos: map[string][]*RiggerRegistry{}}

	regs, err := s.riggerRegistryList()
	if err != nil {
		return nil, err
	}

	for _, reg := range regs {
		switch {
		case reg.Spec.Type == riggerRegistryTypeGit:
			src.register(reg)
		case reg.Name == structs.RiggerGlobalRegistry && reg.Status.Commit != "":
			src.global = reg
		}
	}

	return src, nil
}

// register adds a git registry: at the source it has, and at the one of
// its last sync.
func (src *riggerSources) register(reg *RiggerRegistry) {
	for _, address := range []string{reg.Spec.Source, reg.Status.Source} {
		if address == "" {
			continue
		}

		key := fetch.CanonicalRepository(address)
		if !slices.Contains(src.repos[key], reg) {
			src.repos[key] = append(src.repos[key], reg)
		}
	}
}

// only limits a request that names its registry (an install, a detail) to
// that registry.
func (src *riggerSources) only(ctx context.Context, reg *RiggerRegistry) error {
	src.repos = map[string][]*RiggerRegistry{}

	if reg.Spec.Type == riggerRegistryTypeGlobal {
		src.global = reg

		return src.readGlobal(ctx)
	}

	src.register(reg)

	return nil
}

// readGlobal reads the index of the global registry at the commit of its
// last good sync, when it has one. Only what asks for the global registry
// pays for it and needs the rights on it.
func (src *riggerSources) readGlobal(ctx context.Context) error {
	if src.global == nil || src.globalDir != "" {
		return nil
	}

	ctx, err := src.use(ctx, src.global.Status.Source)
	if err != nil {
		return err
	}

	return src.useGlobal(ctx, src.global)
}

// use checks the use of a repository by the caller, and returns the
// context in which it is fetched.
func (src *riggerSources) use(ctx context.Context, repository string) (context.Context, error) {
	out, err := src.registries.srv.useRiggerRepository(ctx, src.aclObj, repository)
	if err != nil {
		return ctx, src.refuse(err)
	}

	return out, nil
}

// useGlobal points the request at the index of the global registry at the
// commit of its last good sync.
func (src *riggerSources) useGlobal(ctx context.Context, reg *RiggerRegistry) error {
	dir, err := src.registries.fetcher.Checkout(ctx, reg.Status.Source, reg.Status.Commit)
	if err != nil {
		return err
	}

	src.fetcher.SetGlobalRegistry(dir)
	src.globalDir = dir

	return nil
}

// refuse records a refusal and returns it.
func (src *riggerSources) refuse(err error) error {
	src.mu.Lock()
	defer src.mu.Unlock()

	if src.refused == nil {
		src.refused = err
	}

	return err
}

// refusal is the first refusal of the request, nil without one.
func (src *riggerSources) refusal() error {
	src.mu.Lock()
	defer src.mu.Unlock()

	return src.refused
}

// indexed reports whether everything the request fetched is of the program
// or of an index the servers synced.
func (src *riggerSources) indexed() bool {
	src.mu.Lock()
	defer src.mu.Unlock()

	return !src.unindexed
}

// authorize checks what every source but builtin/ needs.
func (src *riggerSources) authorize(req source.Request) error {
	if src.aclObj == nil ||
		src.aclObj.AllowObjectOperation(acl.ObjectClusterNamespace, structs.RiggerRegistryKindRef, acl.ObjectCapabilityRead) {
		return nil
	}

	return src.refuse(structs.NewErrRPCCodedf(http.StatusForbidden,
		"%v: %s needs read on %s", structs.ErrPermissionDenied, req.Source, structs.RiggerRegistryKindRef))
}

// refuseKind refuses a kind of source the servers don't read.
func (src *riggerSources) refuseKind(req source.Request) error {
	return src.refuse(structs.NewErrRPCCodedf(http.StatusBadRequest,
		"%s: the servers fetch only from registered sources, and read no %s:// source: publish it in a git repository and register that as a %s",
		req.Source, req.Kind, structs.RiggerRegistryKindRef))
}

// refuseRepository refuses a repository that is not registered.
func (src *riggerSources) refuseRepository(req source.Request) error {
	return src.refuse(structs.NewErrRPCCodedf(http.StatusBadRequest,
		"%s: the servers fetch only from registered sources, and %s is none: register it (norca object apply of a %s of the type git with that source) and sync it (rigger registry sync <name>), or import a bundle of the stack",
		req.Source, req.Repository, structs.RiggerRegistryKindRef))
}

// Versions lists the versions of a source a server may fetch from.
func (src *riggerSources) Versions(ctx context.Context, req source.Request) ([]source.Available, error) {
	switch req.Kind {
	case source.KindBuiltin:
		return src.fetcher.Versions(ctx, req)
	case source.KindGit:
		return src.gitVersions(ctx, req)
	case source.KindGlobal:
		if err := src.authorize(req); err != nil {
			return nil, err
		}

		if err := src.readGlobal(ctx); err != nil {
			return nil, err
		}

		if src.globalDir == "" {
			return nil, src.refuse(errRiggerRegistryNotSynced(structs.RiggerGlobalRegistry))
		}

		return src.fetcher.Versions(ctx, req)
	default:
		return nil, src.refuseKind(req)
	}
}

// Fetch returns a version of a source a server may fetch from.
func (src *riggerSources) Fetch(ctx context.Context, req source.Request) (source.Fetched, error) {
	switch req.Kind {
	case source.KindBuiltin:
		return src.fetcher.Fetch(ctx, req)
	case source.KindGit:
		return src.gitFetch(ctx, req)
	case source.KindGlobal:
		return src.globalFetch(ctx, req)
	default:
		return source.Fetched{}, src.refuseKind(req)
	}
}

func (src *riggerSources) gitVersions(ctx context.Context, req source.Request) ([]source.Available, error) {
	if err := src.authorize(req); err != nil {
		return nil, err
	}

	ctx, err := src.use(ctx, req.Repository)
	if err != nil {
		return nil, err
	}

	_, tags, err := src.offered(ctx, req)
	if err != nil {
		return nil, err
	}

	out := make([]source.Available, 0, len(tags))
	for _, tag := range tags {
		out = append(out, source.Available{Version: strings.TrimPrefix(tag.Name, req.TagPrefix), Ref: tag.Name, Commit: tag.Commit})
	}

	return out, nil
}

// gitFetch returns a version of a registered repository as the registry
// had it at its last sync, from the mirror of that sync.
func (src *riggerSources) gitFetch(ctx context.Context, req source.Request) (source.Fetched, error) {
	if err := src.authorize(req); err != nil {
		return source.Fetched{}, err
	}

	ctx, err := src.use(ctx, req.Repository)
	if err != nil {
		return source.Fetched{}, err
	}

	keys := src.trustedKeys(req.Repository)

	// A bundle is a source of its own, for what no sync of a registry
	// offers; it keeps the keys of the registries of its repository.
	if _, ok := src.bundledModule(req); ok && !src.isOffered(ctx, req) {
		if got, ok := src.fromBundle(req); ok {
			got.TrustedKeys = keys

			return got, nil
		}
	}

	repository, tags, err := src.offered(ctx, req)
	if err != nil {
		return source.Fetched{}, err
	}

	name := req.TagPrefix + req.Version
	if req.Ref != "" && req.Ref != name {
		return source.Fetched{}, src.refuse(structs.NewErrRPCCodedf(http.StatusBadRequest,
			"%s: the version %s is tagged %s, not %q", req.Source, req.Version, name, req.Ref))
	}

	i := slices.IndexFunc(tags, func(tag fetch.Tag) bool { return tag.Name == name })
	if i < 0 {
		return source.Fetched{}, fmt.Errorf("%s has no tag %s at the last sync of its registry (rigger registry sync fetches newer ones): %w",
			req.Repository, name, fs.ErrNotExist)
	}

	commit := tags[i].Commit
	if req.Commit != "" && req.Commit != commit {
		return source.Fetched{}, fmt.Errorf("%s %s: %w (%s, now %s)", req.Repository, name, fetch.ErrTagMoved, req.Commit, commit)
	}

	dir, err := src.registries.fetcher.Checkout(ctx, repository, commit)
	if err != nil {
		return source.Fetched{}, err
	}

	return source.Fetched{
		FS: os.DirFS(dir), Dir: path.Clean(req.Path), TrustedKeys: keys,
		Origin: source.Origin{Source: req.Source, Version: req.Version, Ref: name, Commit: commit},
	}, nil
}

// isOffered reports whether a synced registry offers the locked version of
// a request at its commit.
func (src *riggerSources) isOffered(ctx context.Context, req source.Request) bool {
	if src.syncedAt(req.Repository) == nil {
		return false
	}

	_, tags, err := src.offered(ctx, req)

	return err == nil && slices.Contains(tags, fetch.Tag{Name: req.TagPrefix + req.Version, Commit: req.Commit})
}

// syncedAt returns the registries with a good sync of the repository.
func (src *riggerSources) syncedAt(repository string) []*RiggerRegistry {
	key := fetch.CanonicalRepository(repository)

	var out []*RiggerRegistry

	for _, reg := range src.repos[key] {
		if reg.Status.Commit != "" && fetch.CanonicalRepository(reg.Status.Source) == key {
			out = append(out, reg)
		}
	}

	return out
}

// offered returns the tags of a request that each synced registry of its
// repository has at the commit of its sync, and the address to read them
// by.
func (src *riggerSources) offered(ctx context.Context, req source.Request) (string, []fetch.Tag, error) {
	regs := src.repos[fetch.CanonicalRepository(req.Repository)]
	if len(regs) == 0 {
		return "", nil, src.refuseRepository(req)
	}

	synced := src.syncedAt(req.Repository)
	if len(synced) == 0 {
		return "", nil, src.refuse(errRiggerRegistryNotSynced(regs[0].Name))
	}

	var out []fetch.Tag

	for i, reg := range synced {
		tags, err := src.registries.fetcher.TagsAt(ctx, reg.Status.Source, reg.Status.Commit, req.TagPrefix)
		if err != nil {
			return "", nil, fmt.Errorf("the registry %q: %w", reg.Name, err)
		}

		if i == 0 {
			out = tags

			continue
		}

		out = slices.DeleteFunc(out, func(tag fetch.Tag) bool { return !slices.Contains(tags, tag) })
	}

	return synced[0].Status.Source, out, nil
}

// trustedKeys are the rules of every registry of a repository: one that
// asks for a signature can't be gone around by a second one that does not.
func (src *riggerSources) trustedKeys(repository string) [][]string {
	var out [][]string

	for _, reg := range src.repos[fetch.CanonicalRepository(repository)] {
		if len(reg.Spec.TrustedKeys) > 0 {
			out = append(out, reg.Spec.TrustedKeys)
		}
	}

	return out
}

// globalFetch returns a version the global registry lists at its last
// sync. The repository is the one the entry lists, never the one a lock
// file says, and no path on the servers.
func (src *riggerSources) globalFetch(ctx context.Context, req source.Request) (source.Fetched, error) {
	if err := src.authorize(req); err != nil {
		return source.Fetched{}, err
	}

	if err := src.readGlobal(ctx); err != nil {
		return source.Fetched{}, err
	}

	listed, err := src.listed(req)
	if err != nil {
		// Only what the index does not list: a listed version that is
		// blocked stays blocked, bundle or not.
		return src.unlistedFetch(ctx, req, err)
	}

	if ctx, err = src.use(ctx, listed.Repository); err != nil {
		return source.Fetched{}, err
	}

	if fetch.LocalRepository(listed.Repository) {
		return source.Fetched{}, src.refuse(structs.NewErrRPCCodedf(http.StatusBadRequest,
			"%s %s: the repository %q of the entry of the global registry is a path on the servers", req.Source, req.Version, listed.Repository))
	}

	// The fetcher reads the repository from the entry. With that of the
	// lock it would fall back to the git source without the listing when
	// the index can't be read: no signature, no digest, no advisory.
	req.Repository = ""

	return src.fetcher.Fetch(ctx, req)
}

// unlistedFetch returns what a bundle has of a version the index does not
// list, else the error that says so.
func (src *riggerSources) unlistedFetch(ctx context.Context, req source.Request, notListed error) (source.Fetched, error) {
	if _, ok := src.bundledModule(req); !ok {
		return source.Fetched{}, notListed
	}

	if _, err := src.use(ctx, req.Repository); err != nil {
		return source.Fetched{}, err
	}

	got, ok := src.fromBundle(req)
	if !ok {
		return source.Fetched{}, notListed
	}

	got.Origin.Repository = req.Repository
	got.TrustedKeys = src.trustedKeys(req.Repository)

	return got, nil
}

// listed returns what the index of the request lists of a version.
func (src *riggerSources) listed(req source.Request) (*index.Version, error) {
	if src.globalDir == "" {
		return nil, src.notSyncedGlobal(req)
	}

	kind := index.KindModule
	if req.Stack {
		kind = index.KindStack
	}

	entry, err := index.ReadEntry(os.DirFS(src.globalDir), kind, req.Namespace, req.Name)
	if err != nil {
		return nil, fmt.Errorf("the global registry at its last sync lists no %s %s: %w", kind, req.Source, err)
	}

	v := entry.Version(req.Version)
	if v == nil {
		return nil, fmt.Errorf("the global registry at its last sync lists no version %s of %s: %w", req.Version, req.Source, fs.ErrNotExist)
	}

	return v, nil
}

// notSyncedGlobal is the error of a global registry without a sync; a
// refusal unless a bundle has what the request locks.
func (src *riggerSources) notSyncedGlobal(req source.Request) error {
	err := errRiggerRegistryNotSynced(structs.RiggerGlobalRegistry)
	if _, ok := src.bundledModule(req); ok {
		return err
	}

	return src.refuse(err)
}

// bundledModule finds the module of an imported bundle that a request
// locks: the same repository, commit and digest.
func (src *riggerSources) bundledModule(req source.Request) (riggerBundledModule, bool) {
	if req.Commit == "" || req.Digest == "" || req.Repository == "" {
		return riggerBundledModule{}, false
	}

	want := fetch.CanonicalRepository(req.Repository)

	i := slices.IndexFunc(src.bundled, func(m riggerBundledModule) bool {
		return m.commit == req.Commit && m.digest == req.Digest && m.repository == want
	})
	if i < 0 {
		return riggerBundledModule{}, false
	}

	return src.bundled[i], true
}

// fromBundle returns what a request locks from the tree of a bundle, and
// counts it as vouched for by no index. The engine checks the digest.
func (src *riggerSources) fromBundle(req source.Request) (source.Fetched, bool) {
	mod, ok := src.bundledModule(req)
	if !ok {
		return source.Fetched{}, false
	}

	src.mu.Lock()
	src.unindexed = true
	src.mu.Unlock()

	return source.Fetched{
		FS: os.DirFS(src.registries.fetcher.BundleDir(mod.repository, mod.commit, mod.dir, mod.digest)), Dir: path.Clean(mod.dir),
		Origin: source.Origin{Source: req.Source, Version: req.Version, Ref: req.Ref, Commit: req.Commit},
	}, true
}

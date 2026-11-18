// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rig

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca-rig/internal/lang"
	"github.com/bacanoo-io/norca-rig/oplog"
	"github.com/bacanoo-io/norca-rig/source"
	"github.com/bacanoo-io/norca-rig/version"
)

// ErrNoFetcher is the error of a module of a source the engine can't fetch.
var ErrNoFetcher = errors.New("the engine has no fetcher of modules (rig.WithFetcher)")

// resolveLocked loads the modules of the releases from sources as the lock
// file of the stack records them (design 8.3): the version, the tag and its
// commit, and the tree digest, which must match.
func (e *Engine) resolveLocked(ctx context.Context, s *lang.Stack) hcl.Diagnostics {
	var diags hcl.Diagnostics

	for _, r := range s.Releases {
		if r.Address == nil {
			continue
		}

		rng := r.Block.DefRange

		entry := s.Lock.Module(r.Source)
		if entry == nil {
			diags = append(diags, diagAt(rng, "Not in the lock file",
				fmt.Sprintf("%s has no entry in %s; run rig stack sync.", r.Source, lang.LockFile)))

			continue
		}

		if err := meets(r.Version, entry.Version); err != nil {
			diags = append(diags, diagAt(rng, "Lock file out of date", err.Error()+"; run rig stack sync."))

			continue
		}

		m, err := e.fetchLocked(ctx, r, entry)
		if err != nil {
			diags = append(diags, diagAt(rng, "Module not loaded", err.Error()+"."))

			continue
		}

		r.Module, r.Locked = m, entry
	}

	return diags
}

// meets checks that a version meets a constraint; an empty constraint
// takes any version.
func meets(constraint, v string) error {
	if constraint == "" {
		return nil
	}

	c, err := version.ParseConstraint(constraint)
	if err != nil {
		return err
	}

	parsed, err := version.Parse(v)
	if err != nil {
		return err
	}

	if !c.Check(parsed) {
		return fmt.Errorf("the locked version %s does not meet %q", v, constraint)
	}

	return nil
}

// fetchLocked fetches the version a lock entry records and checks it.
func (e *Engine) fetchLocked(ctx context.Context, r *lang.StackRelease, entry *lang.LockedModule) (*lang.Module, error) {
	req := *r.Address
	req.Version, req.Ref, req.Commit = entry.Version, entry.Ref, entry.Commit

	m, origin, err := e.fetchModule(ctx, req)
	if err != nil {
		return nil, err
	}

	if origin.Digest != entry.Digest {
		return nil, fmt.Errorf("the content of %s %s is not what the lock file records (%s, now %s)",
			r.Source, entry.Version, entry.Digest, origin.Digest)
	}

	return m, nil
}

// fetchModule fetches and loads the module of a request, and returns its
// origin with its tree digest.
func (e *Engine) fetchModule(ctx context.Context, req source.Request) (*lang.Module, source.Origin, error) {
	if e.fetcher == nil {
		return nil, source.Origin{}, ErrNoFetcher
	}

	ctx, span := oplog.Start(ctx, e.logger, slog.LevelDebug, "module.fetch", "source", req.Source, "version", req.Version)

	m, origin, err := e.fetchAndLoad(ctx, req)
	span.End(ctx, err)

	return m, origin, err
}

func (e *Engine) fetchAndLoad(ctx context.Context, req source.Request) (*lang.Module, source.Origin, error) {
	fetched, err := e.fetcher.Fetch(ctx, req)
	if err != nil {
		return nil, source.Origin{}, err
	}

	prefix := req.Source
	if req.Version != "" {
		prefix += "@" + req.Version
	}

	m, diags := lang.LoadAs(ctx, fetched.FS, fetched.Dir, prefix, e.langLimits())
	if diags.HasErrors() {
		return nil, source.Origin{}, diags
	}

	if want := moduleName(req); want != "" && m.Metadata.Name != want {
		return nil, source.Origin{}, fmt.Errorf("%s holds the module %q, not %q", req.Source, m.Metadata.Name, want)
	}

	digest, err := lang.Digest(m)
	if err != nil {
		return nil, source.Origin{}, err
	}

	origin := fetched.Origin
	origin.Digest = digest

	if origin.Version == "" {
		origin.Version = m.Metadata.Version
	}

	return m, origin, nil
}

// moduleName is the name a module of a source must have: the last element
// of its path in a repository, the name of a built-in module; empty when
// the address does not say.
func moduleName(req source.Request) string {
	if req.Path == "." || req.Path == "" {
		return ""
	}

	return path.Base(req.Path)
}

// SyncStack resolves the versions of the modules of a stack from sources,
// fetches and verifies them, and returns the stack loaded with them and the
// new lock file (design 8.3, rig stack sync). A locked version that still
// meets its constraint is kept unless upgrade is set.
func (e *Engine) SyncStack(ctx context.Context, root fs.FS, dir string, upgrade bool) (*Stack, []byte, hcl.Diagnostics) {
	ctx, span := oplog.Start(ctx, e.logger, slog.LevelInfo, "stack.sync", "dir", dir)

	s, lock, diags := e.syncStack(ctx, root, dir, upgrade)
	span.End(ctx, diagsErr(diags))

	return s, lock, diags
}

func (e *Engine) syncStack(ctx context.Context, root fs.FS, dir string, upgrade bool) (*Stack, []byte, hcl.Diagnostics) {
	s, diags := lang.LoadStack(ctx, root, dir, e.langLimits())
	if diags.HasErrors() {
		return nil, nil, diags
	}

	lock := &lang.Lock{}

	for _, r := range s.Releases {
		if r.Address == nil {
			continue
		}

		if entry := lock.Module(r.Source); entry != nil {
			if err := meets(r.Version, entry.Version); err != nil {
				diags = append(diags, diagAt(r.Block.DefRange, "Constraints differ", fmt.Sprintf(
					"%s is used by more than one release; %s.", r.Source, err)))
			}

			continue
		}

		entry, m, err := e.syncRelease(ctx, s, r, upgrade)
		if err != nil {
			diags = append(diags, diagAt(r.Block.DefRange, "Module not resolved", err.Error()+"."))

			continue
		}

		r.Module, r.Locked = m, entry
		lock.Modules = append(lock.Modules, entry)
	}

	if diags.HasErrors() {
		return nil, nil, diags
	}

	s.Lock = lock

	return &Stack{s: s}, lock.Bytes(), diags
}

// syncRelease resolves the module of one release: the locked version when
// it still meets the constraint, else the newest version that does.
func (e *Engine) syncRelease(ctx context.Context, s *lang.Stack, r *lang.StackRelease, upgrade bool) (*lang.LockedModule, *lang.Module, error) {
	if old := s.Lock.Module(r.Source); old != nil && !upgrade && meets(r.Version, old.Version) == nil {
		m, err := e.fetchLocked(ctx, r, old)
		if err != nil {
			return nil, nil, err
		}

		entry := *old
		entry.Constraint = r.Version

		return &entry, m, nil
	}

	if e.fetcher == nil {
		return nil, nil, ErrNoFetcher
	}

	available, err := e.fetcher.Versions(ctx, *r.Address)
	if err != nil {
		return nil, nil, err
	}

	pick, err := newest(available, r.Version)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", r.Source, err)
	}

	req := *r.Address
	req.Version, req.Ref, req.Commit = pick.Version, pick.Ref, pick.Commit

	m, origin, err := e.fetchModule(ctx, req)
	if err != nil {
		return nil, nil, err
	}

	if err := meets(r.Version, origin.Version); err != nil {
		return nil, nil, err
	}

	return &lang.LockedModule{
		Source: r.Source, Version: origin.Version, Constraint: r.Version, Repository: req.Repository,
		Path: req.Path, Ref: origin.Ref, Commit: origin.Commit, Digest: origin.Digest,
	}, m, nil
}

// errNoVersion is the error of a source with no version that meets a
// constraint.
var errNoVersion = errors.New("no version meets the constraint")

// newest picks the newest available version that meets the constraint. A
// source of one version without a number (a directory, a built-in module)
// is taken as it is; its metadata says the version.
func newest(available []source.Available, constraint string) (source.Available, error) {
	if len(available) == 1 && available[0].Version == "" {
		return available[0], nil
	}

	c := version.Constraint{}

	if constraint != "" {
		parsed, err := version.ParseConstraint(constraint)
		if err != nil {
			return source.Available{}, err
		}

		c = parsed
	} else {
		c, _ = version.ParseConstraint(">= 0.0.0")
	}

	var (
		best   source.Available
		bestV  version.Version
		picked bool
	)

	for _, a := range available {
		v, err := version.Parse(a.Version)
		if err != nil || !c.Check(v) {
			continue
		}

		if !picked || version.Compare(v, bestV) > 0 {
			best, bestV, picked = a, v, true
		}
	}

	if !picked {
		return source.Available{}, fmt.Errorf("%w %q", errNoVersion, constraint)
	}

	return best, nil
}

func diagAt(rng hcl.Range, summary, detail string) *hcl.Diagnostic {
	return &hcl.Diagnostic{Severity: hcl.DiagError, Summary: summary, Detail: detail, Subject: rng.Ptr()}
}

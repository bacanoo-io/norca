// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"slices"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/rigger/internal/eval"
	"github.com/bacanoo-io/norca/rigger/internal/lang"
	"github.com/bacanoo-io/norca/rigger/oplog"
	"github.com/bacanoo-io/norca/rigger/sign"
	"github.com/bacanoo-io/norca/rigger/source"
	"github.com/bacanoo-io/norca/rigger/version"
)

// ErrNoFetcher is the error of a module of a source the engine can't fetch.
var ErrNoFetcher = errors.New("the engine has no fetcher of modules (rigger.WithFetcher)")

// resolveLocked loads the modules of the releases from sources as the lock
// file of the stack records them (design 8.3): the version, the tag and its
// commit, and the tree digest, which must match.
func (e *Engine) resolveLocked(ctx context.Context, s *lang.Stack) hcl.Diagnostics {
	var total int

	return e.resolveCounted(ctx, s, &total)
}

// resolveCounted is resolveLocked of a stack or of the stack of a part;
// total counts the modules of them all against Limits.Modules.
func (e *Engine) resolveCounted(ctx context.Context, s *lang.Stack, total *int) hcl.Diagnostics {
	rules, diags := eval.StackTrust(s, e.limits.FileBytes)
	if diags.HasErrors() {
		return diags
	}

	for _, r := range s.Releases {
		if r.Address == nil && r.Module != nil {
			*total += moduleCount(r.Module)
		}
	}

	for _, r := range s.Releases {
		if *total > e.limits.Modules {
			return append(diags, e.tooManyModules(r.Block.DefRange))
		}

		if r.Address == nil {
			continue
		}

		rng := r.Block.DefRange

		entry := s.Lock.Module(r.Source)
		if entry == nil {
			diags = append(diags, diagAt(rng, "Not in the lock file",
				fmt.Sprintf("%s has no entry in %s; run rigger stack sync.", r.Source, lang.LockFile)))

			continue
		}

		if err := meets(r.Version, entry.Version); err != nil {
			diags = append(diags, diagAt(rng, "Lock file out of date", err.Error()+"; run rigger stack sync."))

			continue
		}

		fm, err := e.fetchLocked(ctx, r, entry)
		if err == nil {
			err = e.trust(rules, r, fm, entry.Signers)
		}

		if err != nil {
			diags = append(diags, diagAt(rng, "Module not loaded", err.Error()+"."))

			continue
		}

		diags = append(diags, advisories(rng, r.Source, fm)...)

		r.Module, r.Locked = fm.module, entry

		*total += moduleCount(fm.module)
		if *total > e.limits.Modules {
			return append(diags, e.tooManyModules(rng))
		}
	}

	return append(diags, e.resolveLockedParts(ctx, s, rules, total)...)
}

// moduleCount is the number of modules of a tree, as often as each is
// called.
func moduleCount(m *lang.Module) int {
	n := 1

	for _, c := range m.Children {
		if c.Module != nil {
			n += moduleCount(c.Module)
		}
	}

	return n
}

// tooManyModules is the diagnostic of a stack over Limits.Modules.
func (e *Engine) tooManyModules(rng hcl.Range) *hcl.Diagnostic {
	return diagAt(rng, "Too many modules", fmt.Sprintf(
		"The stack has more than %d modules with their child modules, the limit of the engine.", e.limits.Modules))
}

// trustRule is the rule of a registry, else the default rule (a trust
// block that names no registry), else none.
func trustRule(rules []eval.TrustRule, registry string) eval.TrustRule {
	var def eval.TrustRule

	for _, rule := range rules {
		switch rule.Registry {
		case "":
			def = rule
		case registry:
			return rule
		}
	}

	return def
}

// fetchedModule is a module fetched from a source.
type fetchedModule struct {
	module     *lang.Module
	origin     source.Origin
	signatures []sign.Signature
	// dir is the directory of the module in what was fetched.
	dir string
	// listed is set for a version the index of a registry lists: it is
	// taken only signed by a key of keys, the keys of its namespace.
	listed   bool
	keys     [][]byte
	warnings []string
}

// advisories are the warnings the index of a registry records on the
// version of a module (design 7.3), shown by sync and plan.
func advisories(rng hcl.Range, src string, fm *fetchedModule) hcl.Diagnostics {
	var diags hcl.Diagnostics

	for _, w := range fm.warnings {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagWarning, Summary: "Advisory",
			Detail:  fmt.Sprintf("%s %s: %s", src, fm.origin.Version, w),
			Subject: rng.Ptr(),
		})
	}

	return diags
}

// ErrNotTrusted is the error of a module the trust of the stack does not
// take (design 8.2).
var ErrNotTrusted = errors.New("not signed by a key the stack trusts")

// trust checks the signatures of a fetched module against the trust of the
// stack: the rule of its registry, else the default rule. A version an
// index lists needs a signature of a key of its namespace or of the rule.
// A built-in module is part of the program and needs none. locked are the
// signers the lock file records, which must still sign. It sets the
// signatures and signers of the release.
func (e *Engine) trust(rules []eval.TrustRule, r *lang.StackRelease, fm *fetchedModule, locked []string) error {
	if r.Address.Kind == source.KindBuiltin {
		return nil
	}

	signers, err := verifyTrust(trustRule(rules, r.Address.Registry), signedContent{
		source: r.Source, version: fm.origin.Version, digest: fm.origin.Digest,
		signatures: fm.signatures, keys: fm.keys, listed: fm.listed,
	}, locked)
	if err != nil {
		return err
	}

	r.Signatures, r.Signers = fm.signatures, signers

	return nil
}

// signedContent is a fetched module or stack and what signs it.
type signedContent struct {
	source, version, digest string
	signatures              []sign.Signature
	// keys are the keys of the namespace of a version an index lists.
	keys   [][]byte
	listed bool
}

// verifyTrust verifies the signatures of fetched content with the keys of
// a trust rule and of its namespace; it returns the keys that sign it. A
// rule that requires a signature, or an index that lists the version,
// needs one; locked are the signers the lock file records, which must
// still sign.
func verifyTrust(rule eval.TrustRule, c signedContent, locked []string) ([]string, error) {
	keys := make([]sign.PublicKey, 0, len(rule.Keys)+len(c.keys))

	for _, text := range rule.Keys {
		k, err := sign.ParsePublicKey([]byte(text))
		if err != nil {
			return nil, fmt.Errorf("a public key of the trust of the stack: %w", err)
		}

		keys = append(keys, k)
	}

	for _, text := range c.keys {
		k, err := sign.ParsePublicKey(text)
		if err != nil {
			return nil, fmt.Errorf("a public key of the namespace of %s: %w", c.source, err)
		}

		keys = append(keys, k)
	}

	signers := sign.Verify(c.digest, c.signatures, keys)

	if (rule.Require || c.listed) && len(signers) == 0 {
		return nil, fmt.Errorf("%s %s: %w", c.source, c.version, ErrNotTrusted)
	}

	for _, id := range locked {
		if !slices.Contains(signers, id) {
			return nil, fmt.Errorf("%s %s: the lock file records a signature of %s, which no longer verifies; run rigger stack sync", c.source, c.version, id)
		}
	}

	return signers, nil
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
func (e *Engine) fetchLocked(ctx context.Context, r *lang.StackRelease, entry *lang.LockedModule) (*fetchedModule, error) {
	req := *r.Address
	req.Version, req.Ref, req.Commit, req.Path = entry.Version, entry.Ref, entry.Commit, entry.Path

	if req.Kind == source.KindGlobal {
		// The repository of the version, for a tree in the cache when the
		// index can't be reached.
		req.Repository = entry.Repository
	}

	fm, err := e.fetchModule(ctx, req)
	if err != nil {
		return nil, err
	}

	if fm.origin.Digest != entry.Digest {
		return nil, fmt.Errorf("the content of %s %s is not what the lock file records (%s, now %s)",
			r.Source, entry.Version, entry.Digest, fm.origin.Digest)
	}

	return fm, nil
}

// fetchModule fetches and loads the module of a request, with its origin,
// its tree digest and its signatures.
func (e *Engine) fetchModule(ctx context.Context, req source.Request) (*fetchedModule, error) {
	if e.fetcher == nil {
		return nil, ErrNoFetcher
	}

	ctx, span := oplog.Start(ctx, e.logger, slog.LevelDebug, "module.fetch", "source", req.Source, "version", req.Version)

	fm, err := e.fetchAndLoad(ctx, req)
	span.End(ctx, err)

	return fm, err
}

func (e *Engine) fetchAndLoad(ctx context.Context, req source.Request) (*fetchedModule, error) {
	fetched, err := e.fetcher.Fetch(ctx, req)
	if err != nil {
		return nil, err
	}

	sigs, err := sign.ReadSignatures(fetched.FS, fetched.Dir)
	if err != nil {
		return nil, err
	}

	prefix := req.Source
	if req.Version != "" {
		prefix += "@" + req.Version
	}

	m, diags := lang.LoadAs(ctx, fetched.FS, fetched.Dir, prefix, e.langLimits())
	if diags.HasErrors() {
		return nil, diags
	}

	if want := moduleName(req); want != "" && m.Metadata.Name != want {
		return nil, fmt.Errorf("%s holds the module %q, not %q", req.Source, m.Metadata.Name, want)
	}

	digest, err := lang.Digest(m)
	if err != nil {
		return nil, err
	}

	origin := fetched.Origin
	origin.Digest = digest

	fm := &fetchedModule{module: m, origin: origin, signatures: sigs, dir: path.Clean(fetched.Dir)}

	if fetched.Listing != nil {
		if err := fm.checkListing(req, fetched.Listing); err != nil {
			return nil, err
		}
	}

	switch {
	case fm.origin.Version == "":
		fm.origin.Version = m.Metadata.Version
	case fm.origin.Version != m.Metadata.Version:
		return nil, fmt.Errorf("%s: the tag %s is version %s, the metadata of the module says %q",
			req.Source, origin.Ref, origin.Version, m.Metadata.Version)
	}

	return fm, nil
}

// checkListing checks a fetched version against what the index lists of it: the
// name and the tree digest; it adds the signatures of the index and the
// keys of the namespace.
func (fm *fetchedModule) checkListing(req source.Request, l *source.Listing) error {
	m := fm.module

	if m.Metadata.Name != l.Name {
		return fmt.Errorf("%s holds the module %q, the index lists %q", req.Source, m.Metadata.Name, l.Name)
	}

	if fm.origin.Digest != l.Digest {
		return fmt.Errorf("the content of %s %s is not what the index lists (%s, now %s)",
			req.Source, m.Metadata.Version, l.Digest, fm.origin.Digest)
	}

	statement, err := sign.Statement(l.Digest, l.Name, m.Metadata.Version)
	if err != nil {
		return err
	}

	for _, s := range l.Signatures {
		fm.signatures = append(fm.signatures, sign.Signature{Payload: statement, Signature: s.Signature})
	}

	fm.listed, fm.keys, fm.warnings = true, l.Keys, l.Warnings

	return nil
}

// moduleName is the name a module of a source must have: the last element
// of its path in a repository, the name of a built-in module; empty when
// the address does not say.
func moduleName(req source.Request) string {
	if req.Kind == source.KindGlobal {
		return req.Name
	}

	if req.Path == "." || req.Path == "" {
		return ""
	}

	return path.Base(req.Path)
}

// SyncStack resolves the versions of the modules of a stack from sources,
// fetches and verifies them, and returns the stack loaded with them and the
// new lock file (design 8.3, rigger stack sync). A locked version that still
// meets its constraint is kept unless upgrade is set.
func (e *Engine) SyncStack(ctx context.Context, root fs.FS, dir string, upgrade bool) (_ *Stack, _ []byte, retDiags hcl.Diagnostics) {
	defer e.recoverDiags(ctx, "stack.sync", &retDiags)

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

	rules, moreDiags := eval.StackTrust(s, e.limits.FileBytes)
	if diags = append(diags, moreDiags...); moreDiags.HasErrors() {
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

		entry, fm, err := e.syncRelease(ctx, s, r, upgrade)
		if err == nil {
			err = e.trust(rules, r, fm, nil)
		}

		if err != nil {
			diags = append(diags, diagAt(r.Block.DefRange, "Module not resolved", err.Error()+"."))

			continue
		}

		diags = append(diags, advisories(r.Block.DefRange, r.Source, fm)...)
		entry.Signers = r.Signers
		r.Module, r.Locked = fm.module, entry
		lock.Modules = append(lock.Modules, entry)
	}

	diags = append(diags, e.syncParts(ctx, s, lock, rules, upgrade)...)

	if diags.HasErrors() {
		return nil, nil, diags
	}

	s.Lock = lock

	return &Stack{s: s}, lock.Bytes(), diags
}

// syncRelease resolves the module of one release: the locked version when
// it still meets the constraint, else the newest version that does.
func (e *Engine) syncRelease(ctx context.Context, s *lang.Stack, r *lang.StackRelease, upgrade bool) (*lang.LockedModule, *fetchedModule, error) {
	if old := s.Lock.Module(r.Source); old != nil && !upgrade && meets(r.Version, old.Version) == nil {
		fm, err := e.fetchLocked(ctx, r, old)
		if err != nil {
			return nil, nil, err
		}

		entry := *old
		entry.Constraint = r.Version

		return &entry, fm, nil
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

	fm, err := e.fetchModule(ctx, req)
	if err != nil {
		return nil, nil, err
	}

	origin := fm.origin
	if err := meets(r.Version, origin.Version); err != nil {
		return nil, nil, err
	}

	repository := req.Repository
	if origin.Repository != "" {
		repository = origin.Repository
	}

	return &lang.LockedModule{
		Source: r.Source, Version: origin.Version, Constraint: r.Version, Repository: repository,
		Path: fm.dir, Ref: origin.Ref, Commit: origin.Commit, Digest: origin.Digest,
	}, fm, nil
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

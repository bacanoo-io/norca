// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger

import (
	"context"
	"fmt"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/rigger/internal/eval"
	"github.com/bacanoo-io/norca/rigger/internal/lang"
	"github.com/bacanoo-io/norca/rigger/source"
)

// resolveLockedParts loads the parts of a stack (design 7.5) as its lock
// file records them: the version and its digest, which must match, signed
// as the trust of the stack asks; and the modules of each part as its own
// lock file pins them.
func (e *Engine) resolveLockedParts(ctx context.Context, s *lang.Stack, rules []eval.TrustRule, total *int) hcl.Diagnostics {
	var diags hcl.Diagnostics

	for _, p := range s.Parts {
		rng := p.Block.DefRange

		entry := s.Lock.Stack(p.Source)
		if entry == nil {
			diags = append(diags, diagAt(rng, "Not in the lock file",
				fmt.Sprintf("%s has no entry in %s; run rigger stack sync.", p.Source, lang.LockFile)))

			continue
		}

		if err := meets(p.Version, entry.Version); err != nil {
			diags = append(diags, diagAt(rng, "Lock file out of date", err.Error()+"; run rigger stack sync."))

			continue
		}

		fs, err := e.fetchLockedStack(ctx, p, entry)
		if err == nil {
			content := fs.signed()
			content.unindexed = p.Address.Kind == source.KindGlobal && fs.listing == nil

			p.Signers, err = verifyTrust(trustRule(rules, p.Address.Registry), content, entry.Signers)
		}

		if err != nil {
			diags = append(diags, diagAt(rng, "Stack not loaded", err.Error()+"."))

			continue
		}

		diags = append(diags, e.resolveCounted(ctx, fs.stack, total)...)
		if *total > e.limits.Modules {
			return diags
		}

		diags = append(diags, fs.advisories(rng)...)
		p.Stack, p.Locked, p.Signatures = fs.stack, entry, fs.signatures
	}

	return diags
}

// fetchLockedStack fetches the version of a part its lock entry records,
// and checks its digest.
func (e *Engine) fetchLockedStack(ctx context.Context, p *lang.StackPart, entry *lang.LockedModule) (*fetchedStack, error) {
	req := *p.Address
	req.Version, req.Ref, req.Commit, req.Digest = entry.Version, entry.Ref, entry.Commit, entry.Digest

	if req.Kind == source.KindGlobal {
		req.Repository = entry.Repository

		// Where the index said it is, for a tree in a cache or a bundle.
		if entry.Path != "" {
			req.Path = entry.Path
		}
	}

	fs, err := e.fetchPublished(ctx, req)
	if err != nil {
		return nil, err
	}

	if fs.origin.Digest != entry.Digest {
		return nil, fmt.Errorf("the content of %s %s is not what the lock file records (%s, now %s)",
			p.Source, entry.Version, entry.Digest, fs.origin.Digest)
	}

	return fs, nil
}

// syncParts resolves the versions of the parts of a stack into the lock:
// the locked version while it meets the constraint, unless upgrade, else
// the newest that does. The modules of a part come from its own lock file.
func (e *Engine) syncParts(ctx context.Context, s *lang.Stack, lock *lang.Lock, rules []eval.TrustRule, upgrade bool) hcl.Diagnostics {
	var diags hcl.Diagnostics

	for _, p := range s.Parts {
		rng := p.Block.DefRange

		if entry := lock.Stack(p.Source); entry != nil {
			if err := meets(p.Version, entry.Version); err != nil {
				diags = append(diags, diagAt(rng, "Constraints differ", fmt.Sprintf(
					"%s is a part more than once; %s.", p.Source, err)))
			}

			continue
		}

		entry, fs, err := e.syncPart(ctx, s, p, upgrade)
		if err == nil {
			p.Signers, err = verifyTrust(trustRule(rules, p.Address.Registry), fs.signed(), nil)
		}

		if err != nil {
			diags = append(diags, diagAt(rng, "Stack not resolved", err.Error()+"."))

			continue
		}

		diags = append(diags, e.resolveLocked(ctx, fs.stack)...)
		diags = append(diags, fs.advisories(rng)...)

		entry.Signers = p.Signers
		p.Stack, p.Locked, p.Signatures = fs.stack, entry, fs.signatures
		lock.Stacks = append(lock.Stacks, entry)
	}

	return diags
}

// syncPart resolves the version of one part.
func (e *Engine) syncPart(ctx context.Context, s *lang.Stack, p *lang.StackPart, upgrade bool) (*lang.LockedModule, *fetchedStack, error) {
	if old := s.Lock.Stack(p.Source); old != nil && !upgrade && meets(p.Version, old.Version) == nil {
		fs, err := e.fetchLockedStack(ctx, p, old)
		if err != nil {
			return nil, nil, err
		}

		entry := *old
		entry.Constraint = p.Version

		return &entry, fs, nil
	}

	fs, err := e.fetchNewestStack(ctx, *p.Address, p.Version)
	if err != nil {
		return nil, nil, err
	}

	o := fs.origin

	return &lang.LockedModule{
		Source: p.Source, Version: o.Version, Constraint: p.Version, Repository: o.Repository,
		Path: o.Path, Ref: o.Ref, Commit: o.Commit, Digest: o.Digest,
	}, fs, nil
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rig

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca-rig/internal/lang"
	"github.com/bacanoo-io/norca-rig/oplog"
	"github.com/bacanoo-io/norca-rig/sign"
	"github.com/bacanoo-io/norca-rig/source"
)

// ErrNotStackSource is the error of an address that names no published
// stack: one of the global registry, or a git repository.
var ErrNotStackSource = errors.New("a published stack comes from the global registry (<namespace>/<name>) or a git repository")

// StackOrigin is where a published stack was fetched from.
type StackOrigin struct {
	Source  string   `json:"source"`
	Version string   `json:"version"`
	Commit  string   `json:"commit,omitempty"`
	Digest  string   `json:"digest"`
	Signers []string `json:"signers,omitempty"`
}

// FetchStack fetches a published stack (design 7.5): the newest version
// of the address that meets the constraint, and loads it with the modules
// its lock file pins. A stack of the global registry is taken only with the
// digest its entry lists and signed by a key of its namespace; the modules
// are verified by the digests of its lock file and its own trust.
func (e *Engine) FetchStack(ctx context.Context, address, constraint string) (*Stack, hcl.Diagnostics) {
	ctx, span := oplog.Start(ctx, e.logger, slog.LevelInfo, "stack.fetch", "source", address, "version", constraint)

	s, diags := e.fetchStack(ctx, address, constraint)
	span.End(ctx, diagsErr(diags))

	return s, diags
}

func (e *Engine) fetchStack(ctx context.Context, address, constraint string) (*Stack, hcl.Diagnostics) {
	req, err := source.Parse(address, nil)
	if err == nil && req.Kind != source.KindGlobal && req.Kind != source.KindGit {
		err = fmt.Errorf("%s: %w", address, ErrNotStackSource)
	}

	if err != nil {
		return nil, errorDiags("Invalid stack source", err)
	}

	if e.fetcher == nil {
		return nil, errorDiags("Stack not fetched", ErrNoFetcher)
	}

	req.Stack = true
	if req.Kind == source.KindGlobal {
		req.Path = "stacks/" + req.Name
	}

	available, err := e.fetcher.Versions(ctx, req)
	if err != nil {
		return nil, errorDiags("Stack not fetched", err)
	}

	pick, err := newest(available, constraint)
	if err != nil {
		return nil, errorDiags("Stack not fetched", fmt.Errorf("%s: %w", address, err))
	}

	req.Version, req.Ref, req.Commit = pick.Version, pick.Ref, pick.Commit

	fetched, err := e.fetcher.Fetch(ctx, req)
	if err != nil {
		return nil, errorDiags("Stack not fetched", err)
	}

	s, diags := lang.LoadStack(ctx, fetched.FS, fetched.Dir, e.langLimits())
	if diags.HasErrors() {
		return nil, diags
	}

	origin, err := checkFetchedStack(s, req, fetched)
	if err != nil {
		return nil, errorDiags("Stack not taken", err)
	}

	diags = append(diags, e.resolveLocked(ctx, s)...)

	for _, w := range listingWarnings(fetched.Listing) {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagWarning, Summary: "Advisory",
			Detail: fmt.Sprintf("%s %s: %s", address, origin.Version, w),
		})
	}

	return &Stack{s: s, origin: origin}, diags
}

func listingWarnings(l *source.Listing) []string {
	if l == nil {
		return nil
	}

	return l.Warnings
}

// checkFetchedStack checks a fetched stack: published, of the name and
// version of its address, and, listed in an index, of the listed digest
// and signed by a key of its namespace.
func checkFetchedStack(s *lang.Stack, req source.Request, fetched source.Fetched) (*StackOrigin, error) {
	if s.Metadata == nil {
		return nil, fmt.Errorf("%s has no metadata.hcl: it is not a published stack", req.Source)
	}

	name := path.Base(fetched.Dir)
	if req.Kind == source.KindGlobal {
		name = req.Name
	}

	if (name != "." && s.Metadata.Name != name) || s.Metadata.Version != req.Version {
		return nil, fmt.Errorf("%s %s holds the stack %s %s", req.Source, req.Version, s.Metadata.Name, s.Metadata.Version)
	}

	digest, err := lang.StackDigest(s)
	if err != nil {
		return nil, err
	}

	origin := &StackOrigin{Source: req.Source, Version: req.Version, Commit: fetched.Origin.Commit, Digest: digest}

	l := fetched.Listing
	if l == nil {
		return origin, nil
	}

	if l.Name != s.Metadata.Name || l.Digest != digest {
		return nil, fmt.Errorf("the content of %s %s is not what the index lists (%s, now %s)", req.Source, req.Version, l.Digest, digest)
	}

	signers, err := verifyListed(l, req.Version)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", req.Source, req.Version, err)
	}

	origin.Signers = signers

	return origin, nil
}

// verifyListed verifies the signatures an index lists of a version with
// the keys of its namespace; it returns the keys that sign it.
func verifyListed(l *source.Listing, version string) ([]string, error) {
	statement, err := sign.Statement(l.Digest, l.Name, version)
	if err != nil {
		return nil, err
	}

	sigs := make([]sign.Signature, 0, len(l.Signatures))
	for _, s := range l.Signatures {
		sigs = append(sigs, sign.Signature{Payload: statement, Signature: s.Signature})
	}

	keys := make([]sign.PublicKey, 0, len(l.Keys))

	for _, text := range l.Keys {
		k, err := sign.ParsePublicKey(text)
		if err != nil {
			return nil, fmt.Errorf("a public key of the namespace: %w", err)
		}

		keys = append(keys, k)
	}

	signers := sign.Verify(l.Digest, sigs, keys)
	if len(signers) == 0 {
		return nil, ErrNotTrusted
	}

	return signers, nil
}

// Origin is where a published stack was fetched from; nil for a stack
// loaded from a directory.
func (s *Stack) Origin() *StackOrigin { return s.origin }

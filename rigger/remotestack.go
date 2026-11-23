// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/rigger/internal/eval"
	"github.com/bacanoo-io/norca/rigger/internal/lang"
	"github.com/bacanoo-io/norca/rigger/oplog"
	"github.com/bacanoo-io/norca/rigger/sign"
	"github.com/bacanoo-io/norca/rigger/source"
)

// ErrNotStackSource is the error of an address that names no published
// stack: one of the global registry, or a git repository.
var ErrNotStackSource = errors.New("a published stack comes from the global registry (<namespace>/<name>) or a git repository")

// StackOrigin is where a published stack was fetched from.
type StackOrigin struct {
	Source     string   `json:"source"`
	Version    string   `json:"version"`
	Repository string   `json:"repository,omitempty"`
	Path       string   `json:"path,omitempty"`
	Ref        string   `json:"ref,omitempty"`
	Commit     string   `json:"commit,omitempty"`
	Digest     string   `json:"digest"`
	Signers    []string `json:"signers,omitempty"`
}

// FetchStack fetches a published stack (design 7.5): the newest version
// of the address that meets the constraint, and loads it with the modules
// its lock file pins. A stack of the global registry is taken only with the
// digest its entry lists and signed by a key of its namespace; the modules
// are verified by the digests of its lock file and its own trust.
func (e *Engine) FetchStack(ctx context.Context, address, constraint string) (_ *Stack, retDiags hcl.Diagnostics) {
	defer e.recoverDiags(ctx, "stack.fetch", &retDiags)

	ctx, span := oplog.Start(ctx, e.logger, slog.LevelInfo, "stack.fetch", "source", source.Redact(address), "version", constraint)

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

	req.Stack = true
	if req.Kind == source.KindGlobal {
		req.Path = "stacks/" + req.Name
	}

	fs, err := e.fetchNewestStack(ctx, req, constraint)
	if err == nil {
		fs.origin.Signers, err = verifyTrust(eval.TrustRule{}, fs.signed(), nil)
	}

	if err != nil {
		return nil, errorDiags("Stack not fetched", err)
	}

	diags := e.resolveLocked(ctx, fs.stack)
	diags = append(diags, fs.advisories(hcl.Range{})...)

	return &Stack{s: fs.stack, origin: fs.origin}, diags
}

// fetchedStack is a published stack fetched from a source.
type fetchedStack struct {
	stack      *lang.Stack
	origin     *StackOrigin
	signatures []sign.Signature
	listing    *source.Listing
}

// signed is what signs the stack.
func (fs *fetchedStack) signed() signedContent {
	c := signedContent{source: fs.origin.Source, version: fs.origin.Version, digest: fs.origin.Digest, signatures: fs.signatures}
	if fs.listing != nil {
		c.keys, c.listed = fs.listing.Keys, true
	}

	return c
}

// advisories are the warnings the index records on the version.
func (fs *fetchedStack) advisories(rng hcl.Range) hcl.Diagnostics {
	var diags hcl.Diagnostics

	if fs.listing == nil {
		return nil
	}

	for _, w := range fs.listing.Warnings {
		d := &hcl.Diagnostic{
			Severity: hcl.DiagWarning, Summary: "Advisory",
			Detail: fmt.Sprintf("%s %s: %s", fs.origin.Source, fs.origin.Version, w),
		}

		if rng.Filename != "" {
			d.Subject = rng.Ptr()
		}

		diags = append(diags, d)
	}

	return diags
}

// fetchNewestStack fetches the newest version of a published stack that
// meets the constraint.
func (e *Engine) fetchNewestStack(ctx context.Context, req source.Request, constraint string) (*fetchedStack, error) {
	if e.fetcher == nil {
		return nil, ErrNoFetcher
	}

	available, err := e.fetcher.Versions(ctx, req)
	if err != nil {
		return nil, err
	}

	pick, err := newest(available, constraint)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", req.Source, err)
	}

	req.Version, req.Ref, req.Commit = pick.Version, pick.Ref, pick.Commit

	return e.fetchPublished(ctx, req)
}

// fetchPublished fetches a version of a published stack and loads it,
// without the modules of its sources: published, of the name and version of
// its address, and, listed in an index, of the listed digest.
func (e *Engine) fetchPublished(ctx context.Context, req source.Request) (*fetchedStack, error) {
	if e.fetcher == nil {
		return nil, ErrNoFetcher
	}

	fetched, err := e.fetcher.Fetch(ctx, req)
	if err != nil {
		return nil, err
	}

	s, diags := lang.LoadStack(ctx, fetched.FS, fetched.Dir, e.langLimits())
	if diags.HasErrors() {
		return nil, diags
	}

	if err := checkPublished(s, req, fetched.Dir); err != nil {
		return nil, err
	}

	digest, err := lang.StackDigest(s)
	if err != nil {
		return nil, err
	}

	sigs, err := sign.ReadSignatures(fetched.FS, fetched.Dir)
	if err != nil {
		return nil, err
	}

	fs := &fetchedStack{
		stack: s, signatures: sigs, listing: fetched.Listing,
		origin: &StackOrigin{
			Source: req.Source, Version: req.Version, Repository: fetchedFrom(req, fetched.Origin), Path: path.Clean(fetched.Dir),
			Ref: fetched.Origin.Ref, Commit: fetched.Origin.Commit, Digest: digest,
		},
	}

	if fetched.Listing != nil {
		if err := fs.checkListing(); err != nil {
			return nil, err
		}
	}

	if err := verifyHostTrust(fetched.TrustedKeys, fs.signed()); err != nil {
		return nil, err
	}

	return fs, nil
}

// checkPublished checks that a fetched stack is the published stack of
// its address: its name, by the address or the directory, and its version.
func checkPublished(s *lang.Stack, req source.Request, dir string) error {
	if s.Metadata == nil {
		return fmt.Errorf("%s has no metadata.hcl: it is not a published stack", req.Source)
	}

	name := path.Base(dir)
	if req.Kind == source.KindGlobal {
		name = req.Name
	}

	if (name != "." && s.Metadata.Name != name) || s.Metadata.Version != req.Version {
		return fmt.Errorf("%s %s holds the stack %s %s", req.Source, req.Version, s.Metadata.Name, s.Metadata.Version)
	}

	return nil
}

// checkListing checks a fetched stack against what the index lists of it,
// and adds the signatures of the index.
func (fs *fetchedStack) checkListing() error {
	l, o := fs.listing, fs.origin

	if l.Name != fs.stack.Metadata.Name || l.Digest != o.Digest {
		return fmt.Errorf("the content of %s %s is not what the index lists (%s, now %s)", o.Source, o.Version, l.Digest, o.Digest)
	}

	statement, err := sign.Statement(l.Digest, l.Name, o.Version)
	if err != nil {
		return err
	}

	for _, ls := range l.Signatures {
		fs.signatures = append(fs.signatures, sign.Signature{Payload: statement, Signature: ls.Signature})
	}

	return nil
}

// Origin is where a published stack was fetched from; nil for a stack
// loaded from a directory.
func (s *Stack) Origin() *StackOrigin { return s.origin }

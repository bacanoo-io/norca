// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/rigger/host"
	"github.com/bacanoo-io/norca/rigger/internal/eval"
	"github.com/bacanoo-io/norca/rigger/oplog"
	"github.com/bacanoo-io/norca/rigger/source"
	"github.com/bacanoo-io/norca/rigger/version"
)

// ModuleOrigin is where a fetched module came from and what verified it.
type ModuleOrigin struct {
	Source     string `json:"source"`
	Version    string `json:"version"`
	Repository string `json:"repository,omitempty"`
	Ref        string `json:"ref,omitempty"`
	Commit     string `json:"commit,omitempty"`
	Digest     string `json:"digest"`
	// Signers are the key IDs of the signatures that verified the digest.
	Signers []string `json:"signers,omitempty"`
	// Signatures is how many signatures the version carries, verified or
	// not: with none of Signers, a version with signatures is signed by
	// keys that nothing here trusts.
	Signatures int `json:"signatures,omitempty"`
	// Warnings are the advisories of the version and, for a version that no
	// key signed, that it is unsigned.
	Warnings []string `json:"warnings,omitempty"`
}

// FetchModule fetches the version of a module that req names exactly and
// loads it for an install on a host that keeps registries
// (design/rig-integration.md 4.6 of norca): the host chooses the version
// from the index of its last sync. A version an index lists (the global
// registry) is taken only with the digest it lists and signed by a key of
// its namespace; with trusted keys (those of a registry repository) a
// signature by one of them is required. A version nothing signs is taken
// with a warning.
func (e *Engine) FetchModule(ctx context.Context, req source.Request, trustedKeys []string) (_ *Module, retDiags hcl.Diagnostics) {
	defer e.recoverDiags(ctx, "module.fetch_verified", &retDiags)

	ctx, span := oplog.Start(ctx, e.logger, slog.LevelInfo, "module.fetch_verified", "source", source.Redact(req.Source), "version", req.Version)

	m, err := e.fetchVerified(ctx, req, trustedKeys)
	span.End(ctx, err)

	if err != nil {
		return nil, errorDiags("Module not fetched", err)
	}

	return m, nil
}

func (e *Engine) fetchVerified(ctx context.Context, req source.Request, trustedKeys []string) (*Module, error) {
	if req.Version == "" && req.Ref == "" {
		return nil, fmt.Errorf("%s: an install fetches an exact version", req.Source)
	}

	fm, err := e.fetchModule(ctx, req)
	if err != nil {
		return nil, err
	}

	return verifiedModule(req, fm, trustedKeys)
}

// verifiedModule is a fetched module once its signatures are verified: a
// version an index lists needs a signature of a key of its namespace, and
// with trusted keys one of them; a version nothing signs gets a warning.
func verifiedModule(req source.Request, fm *fetchedModule, trustedKeys []string) (*Module, error) {
	rule := eval.TrustRule{Require: len(trustedKeys) > 0, Keys: trustedKeys}

	signers, err := verifyTrust(rule, signedContent{
		source: req.Source, version: fm.origin.Version, digest: fm.origin.Digest,
		signatures: fm.signatures, keys: fm.keys, listed: fm.listed,
	}, nil)
	if err != nil {
		return nil, err
	}

	o := &ModuleOrigin{
		Source: req.Source, Version: fm.origin.Version, Repository: fm.origin.Repository,
		Ref: fm.origin.Ref, Commit: fm.origin.Commit, Digest: fm.origin.Digest,
		Signers: signers, Signatures: len(fm.signatures), Warnings: fm.warnings,
	}

	if len(signers) == 0 {
		_, detail := UnsignedWarning("module", req.Source, o.Version, o.Signatures)
		o.Warnings = append(o.Warnings, detail)
	}

	mo := moduleOrigin{module: fm.module, source: req.Source, ref: fm.origin.Ref, commit: fm.origin.Commit, signers: signers}
	for _, s := range fm.signatures {
		mo.signatures = append(mo.signatures, host.Signature{Payload: s.Payload, Signature: s.Signature})
	}

	return &Module{mod: fm.module, fsys: fm.module.FS, origin: &mo, fetched: o}, nil
}

// UnsignedWarning is the summary and the detail of the warning of a
// version of a module or a stack (kind) that no key verified: one without
// signatures is not signed; one with signatures is signed by keys that
// nothing here trusts (no trust, index or host names them), so they are
// not verified. Either way only its commit and tree digest are checked.
func UnsignedWarning(kind, src, v string, signatures int) (summary, detail string) {
	title := strings.ToUpper(kind[:1]) + kind[1:]

	if signatures == 0 {
		return title + " not signed", fmt.Sprintf("%s %s is not signed: only its commit and tree digest are checked", src, v)
	}

	return title + " signature not verified", fmt.Sprintf("%s %s is signed, but by no key trusted here: only its commit and tree digest are checked", src, v)
}

// ErrDirectorySource is the error of the address of a directory where a
// module is fetched by its address: a host loads a directory from its
// files (LoadModuleAt).
var ErrDirectorySource = errors.New("a module in a directory is loaded from its files, not fetched by an address")

// FetchNewestModule fetches the newest version of the module of an address
// that meets the constraint (empty: the newest) and loads it for an
// install, as the rigger program installs a module by its address
// (rigger install <address> -version <constraint>). The version is verified
// as FetchModule verifies it, without keys of the host: one of the global
// registry only with the digest its index lists and signed by a key of its
// namespace; one of git or OCI that no key signed with a warning. A
// built-in module has one version, the one the program ships: it must meet
// the constraint, and needs no signature (the program is its source). The
// diagnostics have the advisories of the global registry on the version and
// the warning of an unsigned version as warnings; the origin of the module
// (Module.Origin) has them too. An address of a directory is refused
// (ErrDirectorySource).
func (e *Engine) FetchNewestModule(ctx context.Context, address, constraint string) (_ *Module, retDiags hcl.Diagnostics) {
	defer e.recoverDiags(ctx, "module.fetch_newest", &retDiags)

	ctx, span := oplog.Start(ctx, e.logger, slog.LevelInfo, "module.fetch_newest", "source", source.Redact(address), "version", constraint)

	m, diags := e.fetchNewestModule(ctx, address, constraint)
	span.End(ctx, diagsErr(diags))

	return m, diags
}

func (e *Engine) fetchNewestModule(ctx context.Context, address, constraint string) (*Module, hcl.Diagnostics) {
	req, err := source.Parse(address, nil)
	if err == nil && req.Kind == source.KindDirectory {
		err = fmt.Errorf("%s: %w", source.Redact(address), ErrDirectorySource)
	}

	if err != nil {
		return nil, errorDiags("Invalid module source", err)
	}

	m, fm, err := e.fetchNewest(ctx, req, constraint)
	if err != nil {
		return nil, errorDiags("Module not fetched", err)
	}

	diags := advisories(hcl.Range{}, req.Source, fm)

	if o := m.fetched; req.Kind != source.KindBuiltin && len(o.Signers) == 0 {
		summary, detail := UnsignedWarning("module", req.Source, o.Version, o.Signatures)
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagWarning, Summary: summary, Detail: detail + "."})
	}

	return m, diags
}

// fetchNewest fetches the newest version of a source that meets the
// constraint, and verifies it.
func (e *Engine) fetchNewest(ctx context.Context, req source.Request, constraint string) (*Module, *fetchedModule, error) {
	if e.fetcher == nil {
		return nil, nil, ErrNoFetcher
	}

	available, err := e.fetcher.Versions(ctx, req)
	if err != nil {
		return nil, nil, err
	}

	pick, err := newest(available, constraint)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", req.Source, err)
	}

	req.Version, req.Ref, req.Commit = pick.Version, pick.Ref, pick.Commit

	fm, err := e.fetchModule(ctx, req)
	if err != nil {
		return nil, nil, err
	}

	if req.Kind != source.KindBuiltin {
		m, err := verifiedModule(req, fm, nil)

		return m, fm, err
	}

	// The one version of a built-in module is the one its metadata says.
	if err := builtinMeets(req.Source, fm.origin.Version, constraint); err != nil {
		return nil, nil, err
	}

	o := &ModuleOrigin{Source: req.Source, Version: fm.origin.Version, Digest: fm.origin.Digest}
	mo := moduleOrigin{module: fm.module, source: req.Source}

	return &Module{mod: fm.module, fsys: fm.module.FS, origin: &mo, fetched: o}, fm, nil
}

// ModuleVersions returns the versions of the module of an address that
// FetchNewestModule chooses among, the newest first: of the global
// registry those its index lists but the blocked ones, of git and OCI the
// tags that are versions. A built-in module has one, the version the
// program ships. An address of a directory is refused (ErrDirectorySource);
// a module the global registry does not list is an error that matches
// fs.ErrNotExist.
func (e *Engine) ModuleVersions(ctx context.Context, address string) (_ []string, retErr error) {
	defer e.recoverErr(ctx, "module.versions", &retErr)

	ctx, span := oplog.Start(ctx, e.logger, slog.LevelDebug, "module.versions", "source", source.Redact(address))

	out, err := e.moduleVersions(ctx, address)
	span.End(ctx, err)

	return out, err
}

func (e *Engine) moduleVersions(ctx context.Context, address string) ([]string, error) {
	req, err := source.Parse(address, nil)
	if err != nil {
		return nil, err
	}

	if req.Kind == source.KindDirectory {
		return nil, fmt.Errorf("%s: %w", source.Redact(address), ErrDirectorySource)
	}

	if e.fetcher == nil {
		return nil, ErrNoFetcher
	}

	if req.Kind == source.KindBuiltin {
		fm, err := e.fetchModule(ctx, req)
		if err != nil {
			return nil, err
		}

		return []string{fm.origin.Version}, nil
	}

	available, err := e.fetcher.Versions(ctx, req)
	if err != nil {
		return nil, err
	}

	return versionsNewestFirst(available), nil
}

// versionsNewestFirst returns the versions of available that are versions,
// each once, the newest first.
func versionsNewestFirst(available []source.Available) []string {
	type parsed struct {
		text string
		v    version.Version
	}

	var vs []parsed

	for _, a := range available {
		v, err := version.Parse(a.Version)
		if err != nil || slices.ContainsFunc(vs, func(p parsed) bool { return version.Compare(p.v, v) == 0 }) {
			continue
		}

		vs = append(vs, parsed{text: a.Version, v: v})
	}

	slices.SortFunc(vs, func(a, b parsed) int { return version.Compare(b.v, a.v) })

	out := make([]string, 0, len(vs))
	for _, p := range vs {
		out = append(out, p.text)
	}

	return out
}

// builtinMeets checks that the version of a built-in module meets a
// constraint; an empty constraint takes it.
func builtinMeets(src, v, constraint string) error {
	if constraint == "" {
		return nil
	}

	c, err := version.ParseConstraint(constraint)
	if err != nil {
		return err
	}

	parsed, err := version.Parse(v)
	if err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}

	if !c.Check(parsed) {
		return fmt.Errorf("%s is version %s in this program: %w %q", src, v, errNoVersion, constraint)
	}

	return nil
}

// Origin returns where a fetched module came from and what verified it; nil
// for a module loaded from files.
func (m *Module) Origin() *ModuleOrigin {
	if m.fetched == nil {
		return nil
	}

	o := *m.fetched

	return &o
}

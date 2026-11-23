// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/rigger/host"
	"github.com/bacanoo-io/norca/rigger/internal/eval"
	"github.com/bacanoo-io/norca/rigger/oplog"
	"github.com/bacanoo-io/norca/rigger/source"
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
		Signers: signers, Warnings: fm.warnings,
	}

	if len(signers) == 0 {
		o.Warnings = append(o.Warnings, fmt.Sprintf("%s %s is not signed: only its commit and tree digest are checked", req.Source, o.Version))
	}

	mo := moduleOrigin{module: fm.module, source: req.Source, ref: fm.origin.Ref, signers: signers}
	for _, s := range fm.signatures {
		mo.signatures = append(mo.signatures, host.Signature{Payload: s.Payload, Signature: s.Signature})
	}

	return &Module{mod: fm.module, fsys: fm.module.FS, origin: &mo, fetched: o}, nil
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

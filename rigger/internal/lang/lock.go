// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package lang

import (
	"slices"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/bacanoo-io/norca/rigger/internal/syntax"
)

// LockFile is the name of the lock file of a stack (design 8.3).
const LockFile = "rigger.lock.hcl"

// Lock is the lock file of a stack: per source, the version it resolved
// to and where that version was found, so that every run installs the same
// content.
type Lock struct {
	Modules []*LockedModule `hcl:"module,block"`
	// Stacks are the published stacks the stack takes as parts (design
	// 7.5); their modules are pinned by their own lock files.
	Stacks []*LockedModule `hcl:"stack,block"`
}

// LockedModule is the entry of one source. Version is empty for a module
// whose metadata has none (design 6.4), so the lock file may leave it out,
// as Bytes does: what pins such a module is its digest.
type LockedModule struct {
	Source     string `hcl:"source,label"`
	Version    string `hcl:"version,optional"`
	Constraint string `hcl:"constraint,optional"`
	Repository string `hcl:"repository,optional"`
	Path       string `hcl:"path,optional"`
	Ref        string `hcl:"ref,optional"`
	Commit     string `hcl:"commit,optional"`
	// Digest is the tree digest of the module (design 8.1).
	Digest string `hcl:"digest"`
	// Signers are the IDs of the keys whose signatures of the digest the
	// trust of the stack took.
	Signers []string `hcl:"signers,optional"`
}

// Module returns the entry of a source, nil for none.
func (l *Lock) Module(source string) *LockedModule {
	if l == nil {
		return nil
	}

	for _, m := range l.Modules {
		if m.Source == source {
			return m
		}
	}

	return nil
}

// Stack returns the entry of the source of a part, nil for none.
func (l *Lock) Stack(source string) *LockedModule {
	if l == nil {
		return nil
	}

	for _, m := range l.Stacks {
		if m.Source == source {
			return m
		}
	}

	return nil
}

// ParseLock parses a lock file.
func ParseLock(src []byte) (*Lock, hcl.Diagnostics) {
	file, diags := syntax.ParseConfig(src, LockFile)
	if diags.HasErrors() {
		return nil, diags
	}

	var l Lock

	return &l, gohcl.DecodeBody(file.Body, nil, &l)
}

// Bytes writes the lock file, the entries by source.
func (l *Lock) Bytes() []byte {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	body.AppendUnstructuredTokens(hclwrite.Tokens{{
		Type:  hclsyntax.TokenComment,
		Bytes: []byte("# Written by rigger stack sync: the versions of the modules of the stack and\n# where they were found. Kept in version control; not edited.\n"),
	}})

	writeEntries(body, "module", l.Modules)
	writeEntries(body, "stack", l.Stacks)

	return hclwrite.Format(f.Bytes())
}

// writeEntries writes the entries of a block type, by source.
func writeEntries(body *hclwrite.Body, typ string, entries []*LockedModule) {
	entries = slices.Clone(entries)
	slices.SortFunc(entries, func(a, b *LockedModule) int {
		switch {
		case a.Source < b.Source:
			return -1
		case a.Source > b.Source:
			return 1
		default:
			return 0
		}
	})

	for _, m := range entries {
		body.AppendNewline()

		b := body.AppendNewBlock(typ, []string{m.Source}).Body()
		for _, kv := range [][2]string{
			{"version", m.Version},
			{"constraint", m.Constraint},
			{"repository", m.Repository},
			{"path", m.Path},
			{"ref", m.Ref},
			{"commit", m.Commit},
			{"digest", m.Digest},
		} {
			if kv[1] != "" {
				b.SetAttributeValue(kv[0], cty.StringVal(kv[1]))
			}
		}

		if len(m.Signers) > 0 {
			signers := make([]cty.Value, len(m.Signers))
			for i, s := range m.Signers {
				signers[i] = cty.StringVal(s)
			}

			b.SetAttributeValue("signers", cty.ListVal(signers))
		}
	}
}

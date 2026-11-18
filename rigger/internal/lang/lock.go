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
)

// LockFile is the name of the lock file of a stack (design 8.3).
const LockFile = "rig.lock.hcl"

// Lock is the lock file of a stack: per source, the version it resolved
// to and where that version was found, so that every run installs the same
// content.
type Lock struct {
	Modules []*LockedModule `hcl:"module,block"`
}

// LockedModule is the entry of one source.
type LockedModule struct {
	Source     string `hcl:"source,label"`
	Version    string `hcl:"version"`
	Constraint string `hcl:"constraint,optional"`
	Repository string `hcl:"repository,optional"`
	Path       string `hcl:"path,optional"`
	Ref        string `hcl:"ref,optional"`
	Commit     string `hcl:"commit,optional"`
	// Digest is the tree digest of the module (design 8.1).
	Digest string `hcl:"digest"`
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

// ParseLock parses a lock file.
func ParseLock(src []byte) (*Lock, hcl.Diagnostics) {
	file, diags := hclsyntax.ParseConfig(src, LockFile, hcl.InitialPos)
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
		Bytes: []byte("# Written by rig stack sync: the versions of the modules of the stack and\n# where they were found (design 8.3). Kept in version control; not edited.\n"),
	}})

	modules := slices.Clone(l.Modules)
	slices.SortFunc(modules, func(a, b *LockedModule) int {
		switch {
		case a.Source < b.Source:
			return -1
		case a.Source > b.Source:
			return 1
		default:
			return 0
		}
	})

	for _, m := range modules {
		body.AppendNewline()

		b := body.AppendNewBlock("module", []string{m.Source}).Body()
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
	}

	return hclwrite.Format(f.Bytes())
}

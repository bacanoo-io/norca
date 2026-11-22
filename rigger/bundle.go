// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger

import (
	"context"
	"io/fs"
	"slices"

	"github.com/bacanoo-io/norca/rigger/internal/lang"
	"github.com/bacanoo-io/norca/rigger/source"
)

// SourcedModule is a module of a stack from a git source, as the lock file
// pins it, with the files of its version: what a bundle carries for an
// install without a network (design 7.1).
type SourcedModule struct {
	Source     string   `json:"source"`
	Version    string   `json:"version"`
	Repository string   `json:"repository"`
	Path       string   `json:"path"`
	Ref        string   `json:"ref"`
	Commit     string   `json:"commit"`
	Digest     string   `json:"digest"`
	Signers    []string `json:"signers,omitempty"`
	// Dirs are the directories of the repository the module and its child
	// modules are in; Root holds them.
	Dirs []string `json:"dirs"`
	Root fs.FS    `json:"-"`
}

// SourcedModules returns the modules of the stack from git sources and
// the global registry, each version once, with the published stacks of its
// parts and their modules.
func (s *Stack) SourcedModules() []SourcedModule {
	var out []SourcedModule

	add := func(m SourcedModule) {
		if !slices.ContainsFunc(out, m.isSame) {
			out = append(out, m)
		}
	}

	stacks := []*lang.Stack{s.s}

	for _, p := range s.s.Parts {
		if p.Stack == nil || p.Locked == nil {
			continue
		}

		// The published stack of a part, with its own modules.
		l := p.Locked
		add(SourcedModule{
			Source: l.Source, Version: l.Version, Repository: l.Repository, Path: l.Path, Ref: l.Ref,
			Commit: l.Commit, Digest: l.Digest, Signers: l.Signers, Dirs: []string{p.Stack.Dir}, Root: p.Stack.Root,
		})

		stacks = append(stacks, p.Stack)
	}

	for _, st := range stacks {
		for _, r := range st.Releases {
			if r.Address == nil || (r.Address.Kind != source.KindGit && r.Address.Kind != source.KindGlobal) || r.Locked == nil || r.Module == nil {
				continue
			}

			l := r.Locked
			add(SourcedModule{
				Source: l.Source, Version: l.Version, Repository: l.Repository, Path: l.Path, Ref: l.Ref,
				Commit: l.Commit, Digest: l.Digest, Signers: l.Signers, Dirs: moduleDirs(r.Module), Root: r.Module.Root,
			})
		}
	}

	return out
}

// isSame reports whether two are one version of one module.
func (m SourcedModule) isSame(o SourcedModule) bool {
	return o.Repository == m.Repository && o.Commit == m.Commit && o.Path == m.Path
}

func moduleDirs(m *lang.Module) []string {
	dirs := []string{m.Dir}

	for _, c := range m.Children {
		for _, d := range moduleDirs(c.Module) {
			if !slices.Contains(dirs, d) {
				dirs = append(dirs, d)
			}
		}
	}

	slices.Sort(dirs)

	return dirs
}

// ModuleDigest loads the module in the directory dir of root and returns
// its tree digest: how an imported bundle is checked.
func (e *Engine) ModuleDigest(ctx context.Context, root fs.FS, dir string) (_ string, retErr error) {
	defer e.recoverErr(ctx, "module.digest", &retErr)

	m, diags := lang.LoadAt(ctx, root, dir, e.langLimits())
	if diags.HasErrors() {
		return "", diags
	}

	return lang.Digest(m)
}

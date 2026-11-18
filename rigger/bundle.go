// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rig

import (
	"context"
	"io/fs"
	"slices"

	"github.com/bacanoo-io/norca-rig/internal/lang"
	"github.com/bacanoo-io/norca-rig/source"
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

// SourcedModules returns the modules of the stack from git sources, each
// version once.
func (s *Stack) SourcedModules() []SourcedModule {
	var out []SourcedModule

	for _, r := range s.s.Releases {
		if r.Address == nil || (r.Address.Kind != source.KindGit && r.Address.Kind != source.KindGlobal) || r.Locked == nil || r.Module == nil {
			continue
		}

		if slices.ContainsFunc(out, func(m SourcedModule) bool { return m.Commit == r.Locked.Commit && m.Path == r.Locked.Path }) {
			continue
		}

		l := r.Locked
		out = append(out, SourcedModule{
			Source: l.Source, Version: l.Version, Repository: l.Repository, Path: l.Path, Ref: l.Ref,
			Commit: l.Commit, Digest: l.Digest, Signers: l.Signers, Dirs: moduleDirs(r.Module), Root: r.Module.Root,
		})
	}

	return out
}

// moduleDirs are the directories of a module and of its child modules.
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
func (e *Engine) ModuleDigest(ctx context.Context, root fs.FS, dir string) (string, error) {
	m, diags := lang.LoadAt(ctx, root, dir, e.langLimits())
	if diags.HasErrors() {
		return "", diags
	}

	return lang.Digest(m)
}

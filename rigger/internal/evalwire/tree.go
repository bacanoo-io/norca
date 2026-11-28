// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package evalwire

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"testing/fstest"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/rigger/internal/lang"
)

// ModuleTree is a module as files: the directories of the module and of
// its child modules, read from the root it was loaded from, its directory
// there and the prefix it was loaded with (lang.LoadAs).
type ModuleTree struct {
	Files  map[string][]byte `json:"files"`
	Dir    string            `json:"dir"`
	Prefix string            `json:"prefix,omitempty"`
}

// StackTree is a stack as files: the directories of the stack and of its
// local modules, the modules of its other sources by release, and its
// parts.
type StackTree struct {
	Files    map[string][]byte      `json:"files"`
	Dir      string                 `json:"dir"`
	Releases map[string]*ModuleTree `json:"releases,omitempty"`
	Parts    map[string]*StackTree  `json:"parts,omitempty"`
}

// ErrNotLoaded is a release or a part whose module or stack the engine has
// not attached: it can't be sent.
var ErrNotLoaded = errors.New("not loaded")

// CollectModule reads the files of a loaded module.
func CollectModule(m *lang.Module) (*ModuleTree, error) {
	t := &ModuleTree{Files: map[string][]byte{}, Dir: m.Dir, Prefix: m.Prefix}
	if err := collectModuleDirs(m.Root, m, t.Files); err != nil {
		return nil, err
	}

	return t, nil
}

// collectModuleDirs reads the directory of m and of its children from
// root into files.
func collectModuleDirs(root fs.FS, m *lang.Module, files map[string][]byte) error {
	if err := collectDir(root, m.Dir, files); err != nil {
		return err
	}

	for _, c := range m.Children {
		if c.Module == nil {
			continue
		}

		if err := collectModuleDirs(root, c.Module, files); err != nil {
			return err
		}
	}

	return nil
}

// CollectStack reads the files of a loaded stack whose modules and parts
// the engine attached.
func CollectStack(s *lang.Stack) (*StackTree, error) {
	t := &StackTree{Files: map[string][]byte{}, Dir: s.Dir}
	if err := collectDir(s.Root, s.Dir, t.Files); err != nil {
		return nil, err
	}

	for _, r := range s.Releases {
		if r.Module == nil {
			return nil, fmt.Errorf("the module of the release %s: %w", r.Name, ErrNotLoaded)
		}

		if r.Address == nil {
			// A local module is loaded with the stack, from its root.
			if err := collectModuleDirs(s.Root, r.Module, t.Files); err != nil {
				return nil, err
			}

			continue
		}

		mt, err := CollectModule(r.Module)
		if err != nil {
			return nil, fmt.Errorf("the module of the release %s: %w", r.Name, err)
		}

		if t.Releases == nil {
			t.Releases = map[string]*ModuleTree{}
		}

		t.Releases[r.Name] = mt
	}

	for _, p := range s.Parts {
		if p.Stack == nil {
			return nil, fmt.Errorf("the stack of the part %s: %w", p.Name, ErrNotLoaded)
		}

		pt, err := CollectStack(p.Stack)
		if err != nil {
			return nil, fmt.Errorf("the part %s: %w", p.Name, err)
		}

		if t.Parts == nil {
			t.Parts = map[string]*StackTree{}
		}

		t.Parts[p.Name] = pt
	}

	return t, nil
}

// collectDir reads the files below dir of root; a hidden path is no
// content of a module (lang).
func collectDir(root fs.FS, dir string, files map[string][]byte) error {
	return fs.WalkDir(root, path.Clean(dir), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if p != path.Clean(dir) && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}

			return nil
		}

		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}

		if _, ok := files[p]; ok {
			return nil
		}

		data, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}

		files[p] = data

		return nil
	})
}

// mapFS is the files of a tree as a file system.
func mapFS(files map[string][]byte) fstest.MapFS {
	out := make(fstest.MapFS, len(files))
	for name, data := range files {
		out[name] = &fstest.MapFile{Data: data, Mode: 0o444}
	}

	return out
}

// Load loads the module of a tree as the engine loaded it.
func (t *ModuleTree) Load(ctx context.Context, limits lang.Limits) (*lang.Module, hcl.Diagnostics) {
	return lang.LoadAs(ctx, mapFS(t.Files), t.Dir, t.Prefix, limits)
}

// Load loads the stack of a tree, and attaches the modules of its other
// sources and its parts, as the engine did.
func (t *StackTree) Load(ctx context.Context, limits lang.Limits) (*lang.Stack, hcl.Diagnostics) {
	s, diags := lang.LoadStack(ctx, mapFS(t.Files), t.Dir, limits)
	if diags.HasErrors() {
		return s, diags
	}

	for _, r := range s.Releases {
		if r.Address == nil {
			continue
		}

		mt, ok := t.Releases[r.Name]
		if !ok {
			return s, append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Module not sent",
				Detail: "The module of the release " + r.Name + " is not in the files of the evaluation.",
			})
		}

		m, moreDiags := mt.Load(ctx, limits)
		diags = append(diags, moreDiags...)

		if moreDiags.HasErrors() {
			return s, diags
		}

		r.Module = m
	}

	for _, p := range s.Parts {
		pt, ok := t.Parts[p.Name]
		if !ok {
			return s, append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Part not sent",
				Detail: "The stack of the part " + p.Name + " is not in the files of the evaluation.",
			})
		}

		ps, moreDiags := pt.Load(ctx, limits)
		diags = append(diags, moreDiags...)

		if moreDiags.HasErrors() {
			return s, diags
		}

		p.Stack = ps
	}

	return s, diags
}

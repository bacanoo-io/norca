// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// goModule is a Go module linked into one or more binaries.
type goModule struct {
	Path    string
	Version string
	// ReplacePath and ReplaceVersion name the module that replaces this one
	// (a fork); its code is what gets linked.
	ReplacePath    string
	ReplaceVersion string
	// Dir is the directory of the (replacement) module's code.
	Dir string
	// Sum is the go.sum hash of the linked code, when known.
	Sum string
	// PkgDirs are the directories of the packages linked from the module.
	PkgDirs map[string]bool
	// UsedIn are the names of the binaries that link the module.
	UsedIn map[string]bool

	Licenses []textFile
	Notices  []textFile
	// IDs are the recognized license identifiers.
	IDs []string
	// Expr is the license expression shown for the module.
	Expr string
	// Override is the reviewed decision applied, if any.
	Override *override
	// CopyDir is set for a source copy (see sourceCopies): its directory,
	// relative to the repository root.
	CopyDir string
}

// key identifies a module version.
func (m *goModule) key() string { return m.Path + "@" + m.Version }

// stdlib is the pseudo module of the Go standard library.
const stdlib = "std"

type listPackage struct {
	ImportPath string
	Dir        string
	Standard   bool
	Module     *listModule
}

type listModule struct {
	Path    string
	Version string
	Dir     string
	Main    bool
	Replace *listModule
}

// baseEnv returns the caller's environment without the target variables and
// without GOROOT (which `go run` may set), so that the go command uses the
// toolchain selected by the module's go.mod.
func baseEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		switch strings.ToUpper(strings.SplitN(kv, "=", 2)[0]) {
		case "GOOS", "GOARCH", "CGO_ENABLED", "GOROOT":
			continue
		}
		env = append(env, kv)
	}
	return env
}

// goEnv returns the environment for running the go command for a target.
func goEnv(goos, goarch string, cgo bool) []string {
	c := "0"
	if cgo {
		c = "1"
	}
	return append(baseEnv(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED="+c)
}

func runGo(dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, stderr.String())
	}
	return out, nil
}

// listDeps runs `go list -deps` for the main package of a binary on a
// target and adds the modules found to mods (keyed by goModule.key; the
// standard library under the key stdlib).
func listDeps(root string, b binary, goos, goarch string, cgo bool, tags []string, pkg string, mods map[string]*goModule) error {
	dir := filepath.Join(root, filepath.FromSlash(b.ModuleDir))
	args := []string{"list", "-deps", "-json=ImportPath,Dir,Standard,Module"}
	if len(tags) > 0 {
		args = append(args, "-tags", strings.Join(tags, ","))
	}
	args = append(args, pkg)
	out, err := runGo(dir, goEnv(goos, goarch, cgo), args...)
	if err != nil {
		return err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listPackage
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		if p.Standard {
			m := mods[stdlib]
			if m == nil {
				m = &goModule{Path: stdlib, PkgDirs: map[string]bool{}, UsedIn: map[string]bool{}}
				mods[stdlib] = m
			}
			m.UsedIn[b.Name] = true
			continue
		}
		if p.Module == nil {
			return fmt.Errorf("package %s has no module", p.ImportPath)
		}
		if p.Module.Main || isLocal(absRoot, p.Module) {
			// Norca's own code (the main module and the api and
			// controller modules replaced by local directories), except
			// third-party source copied into it.
			if c := findSourceCopy(p.ImportPath); c != nil {
				m := mods[c.Path+"@"+c.Version]
				if m == nil {
					m = &goModule{
						Path:    c.Path,
						Version: c.Version,
						Dir:     filepath.Join(absRoot, filepath.FromSlash(c.Dir)),
						PkgDirs: map[string]bool{},
						UsedIn:  map[string]bool{},
						CopyDir: c.Dir,
					}
					mods[m.key()] = m
				}
				m.PkgDirs[p.Dir] = true
				m.UsedIn[b.Name] = true
			}
			continue
		}
		m := mods[p.Module.Path+"@"+p.Module.Version]
		if m == nil {
			m = &goModule{
				Path:    p.Module.Path,
				Version: p.Module.Version,
				Dir:     p.Module.Dir,
				PkgDirs: map[string]bool{},
				UsedIn:  map[string]bool{},
			}
			if r := p.Module.Replace; r != nil {
				m.ReplacePath, m.ReplaceVersion, m.Dir = r.Path, r.Version, r.Dir
			}
			mods[m.key()] = m
		}
		if m.Dir == "" {
			return fmt.Errorf("module %s is not in the module cache", m.key())
		}
		m.PkgDirs[p.Dir] = true
		m.UsedIn[b.Name] = true
	}
	return nil
}

// isLocal reports whether a module is replaced by a directory inside the
// repository.
func isLocal(absRoot string, m *listModule) bool {
	if m.Replace == nil || m.Replace.Version != "" {
		return false
	}
	rel, err := filepath.Rel(absRoot, m.Replace.Dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// goRoot returns GOROOT of the toolchain selected for a module directory.
func goRoot(dir string) (string, error) {
	out, err := runGo(dir, baseEnv(), "env", "GOROOT")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// collectGoModules lists the modules of every binary on every platform.
func collectGoModules(root string) (map[string]*goModule, error) {
	mods := map[string]*goModule{}
	for _, b := range binaries {
		for _, p := range platforms {
			tags := append(append([]string{}, b.Tags...), p.Tags...)
			if err := listDeps(root, b, p.GOOS, p.GOARCH, b.CGO && p.CGO, tags, b.Package, mods); err != nil {
				return nil, err
			}
		}
	}
	return mods, nil
}

// resolveLicenses reads and classifies the license and NOTICE files of the
// modules and applies the allow-list and the overrides. It returns the
// problems found.
func resolveGoLicenses(root string, mods map[string]*goModule) ([]string, error) {
	var problems []string
	for _, k := range sortedModKeys(mods) {
		m := mods[k]
		if m.Path == stdlib {
			gr, err := goRoot(filepath.Join(root, "nomad"))
			if err != nil {
				return nil, err
			}
			b, err := os.ReadFile(filepath.Join(gr, "LICENSE"))
			if err != nil {
				return nil, err
			}
			m.Licenses = []textFile{{Path: "LICENSE", Text: normalizeText(string(b))}}
			m.IDs = licenseIDs(m.Licenses)
			m.Expr = strings.Join(m.IDs, " AND ")
			continue
		}
		dirs := sortedKeys(m.PkgDirs)
		var err error
		if m.Licenses, err = collectFiles(m.Dir, dirs, licenseFileRe); err != nil {
			return nil, err
		}
		if m.Notices, err = collectFiles(m.Dir, dirs, noticeFileRe); err != nil {
			return nil, err
		}
		m.IDs = licenseIDs(m.Licenses)
		m.Expr = strings.Join(m.IDs, " AND ")
		name, version := m.Path, m.Version
		if m.ReplacePath != "" {
			name, version = m.ReplacePath, m.ReplaceVersion
		}
		if o := findOverride("go", name, version); o != nil {
			m.Override = o
			m.Expr = o.License
			applyExcerpt(o, m.Licenses)
			if !o.Pending && !o.Exception && !exprAllowed(o.License, allowedGo) {
				problems = append(problems, fmt.Sprintf("%s: override license %q is not on the allow-list", m.key(), o.License))
			}
			continue
		}
		if len(m.IDs) == 0 {
			problems = append(problems, fmt.Sprintf("%s: no recognized license (license files: %s)", m.key(), filePaths(m.Licenses)))
			continue
		}
		for _, id := range m.IDs {
			if !allowedGo[id] {
				problems = append(problems, fmt.Sprintf("%s: license %s is not on the allow-list (license files: %s)", m.key(), id, filePaths(m.Licenses)))
			}
		}
	}
	return problems, nil
}

func filePaths(files []textFile) string {
	if len(files) == 0 {
		return "none"
	}
	var s []string
	for _, f := range files {
		s = append(s, f.Path)
	}
	return strings.Join(s, ", ")
}

// sortedModKeys returns the keys of mods sorted by module path and version,
// with the standard library first.
func sortedModKeys(mods map[string]*goModule) []string {
	keys := make([]string, 0, len(mods))
	for k := range mods {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := mods[keys[i]], mods[keys[j]]
		if (a.Path == stdlib) != (b.Path == stdlib) {
			return a.Path == stdlib
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Version < b.Version
	})
	return keys
}

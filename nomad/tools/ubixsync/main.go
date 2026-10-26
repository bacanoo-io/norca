// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Command ubixsync adapts the uBixVault source tree imported with git subtree
// under nomad/secretstore/ubix to the Norca module, and checks that it stays
// adapted.
//
//	ubixsync -root <repo>          adapt the tree in place
//	ubixsync -root <repo> -check   fail if the tree is not adapted
//
// uBixVault (https://github.com/cwolsen7905/ubixvault, BSD-3-Clause) is a
// module of its own whose packages are Go internal packages. Norca imports it
// as source (see docs/design/secrets-plan.md, "Implementation basis") and
// applies only mechanical changes, all made by this command, so that a later
// `git subtree pull` stays easy:
//
//  1. Everything Norca does not use is removed: the upstream go.mod and go.sum
//     (the tree is part of the nomad module, not a nested module), the server
//     binary, the HTTP API, the auth methods, the storage backends other than
//     the interface and the in-memory backend, and the build and deployment
//     files. The packages that are kept are listed in keepPackages; anything
//     upstream adds later is dropped unless it is added there.
//  2. Import paths are rewritten from github.com/cwolsen7905/ubixvault/... to
//     github.com/hashicorp/nomad/secretstore/ubix/....
//  3. A thin facade is generated for each package in exportPackages under
//     nomad/secretstore/ubix/export/<pkg>: type aliases and references to the
//     exported constants, error variables and functions of the internal
//     package. The Go internal rule only lets packages below
//     nomad/secretstore/ubix import ubix/internal/...; the facades are the
//     only way the rest of Norca reaches uBixVault.
//
// Updating to a new upstream release:
//
//	git fetch --no-tags https://github.com/cwolsen7905/ubixvault refs/tags/vX.Y.Z
//	git subtree pull --prefix=nomad/secretstore/ubix <commit of vX.Y.Z>
//	# on conflicts: take the upstream side of every conflicted file
//	#   git checkout --theirs -- <files>; files deleted here and changed
//	#   upstream are removed again by the next step
//	go -C nomad/tools run ./ubixsync -root <repo>
//	git add -A nomad/secretstore/ubix && git commit
//
// and update the version in nomad/tools/thirdparty (sourceCopies).
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	// subtreeDir is the git subtree prefix, relative to the repository root.
	subtreeDir = "nomad/secretstore/ubix"
	// upstreamModule is the module path of uBixVault.
	upstreamModule = "github.com/cwolsen7905/ubixvault"
	// norcaPath is the import path of subtreeDir in the nomad module.
	norcaPath = "github.com/hashicorp/nomad/secretstore/ubix"
	// exportDir is the directory of the generated facades, relative to
	// subtreeDir. It is Norca's own and never exists upstream.
	exportDir = "export"
	// exportFile is the name of the generated file in each facade package.
	exportFile = "ubix_export.go"
)

// keepFiles are the top-level files kept from the upstream tree.
var keepFiles = []string{
	"CHANGELOG.md", "CODE_OF_CONDUCT.md", "CONTRIBUTING.md", "LICENSE", "README.md", "SECURITY.md",
}

// keepDirs are the top-level directories kept as a whole (documentation).
var keepDirs = []string{"docs"}

// keepPackages are the packages kept under internal/. They are the parts of
// uBixVault the plan uses (barrier, shamir, seal, core, kv, transit,
// database, audit) and what they import (snapshot, token, storage);
// storagetest holds the shared HA test double used by the core's tests.
var keepPackages = []string{
	"audit", "barrier", "core", "database", "kv", "seal", "shamir", "snapshot",
	"storage", "storage/storagetest", "token", "transit",
}

// dropFiles are files removed from kept packages: the file and MySQL storage
// backends and the retry wrapper, which links the MySQL driver.
var dropFiles = regexp.MustCompile(`^internal/storage/(file|mysql|retry)[^/]*\.go$`)

// exportPackages are the internal packages that get a facade.
var exportPackages = []string{
	"audit", "barrier", "core", "database", "kv", "seal", "shamir", "storage", "transit",
}

func main() {
	root := flag.String("root", ".", "repository root")
	check := flag.Bool("check", false, "only check that the tree is adapted; change nothing")
	flag.Parse()

	problems, err := run(*root, *check)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ubixsync:", err)
		os.Exit(1)
	}
	if len(problems) > 0 {
		fmt.Fprintf(os.Stderr, "ubixsync: %s is not adapted to the Norca module "+
			"(run 'go -C nomad/tools run ./ubixsync -root <repo>'):\n  %s\n",
			subtreeDir, strings.Join(problems, "\n  "))
		os.Exit(1)
	}
	if *check {
		fmt.Fprintf(os.Stderr, "%s is adapted to the Norca module\n", subtreeDir)
	}
}

// run adapts (or, with check, checks) the tree below root/subtreeDir. In
// check mode it returns the differences found; otherwise it returns no
// problems.
func run(root string, check bool) ([]string, error) {
	dir := filepath.Join(root, filepath.FromSlash(subtreeDir))
	if _, err := os.Stat(filepath.Join(dir, "LICENSE")); err != nil {
		return nil, fmt.Errorf("%s does not look like the uBixVault tree: %w", dir, err)
	}
	var problems []string

	// 1. Remove what Norca does not use.
	remove, err := unusedPaths(dir)
	if err != nil {
		return nil, err
	}
	for _, rel := range remove {
		if check {
			problems = append(problems, "not used by Norca, remove: "+rel)
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			return nil, err
		}
	}
	if check && len(remove) > 0 {
		// The rest of the check would only report follow-up problems.
		return problems, nil
	}

	// 2. Rewrite the import paths.
	goFiles, err := listGoFiles(dir)
	if err != nil {
		return nil, err
	}
	for _, rel := range goFiles {
		if strings.HasPrefix(rel, exportDir+"/") {
			continue
		}
		file := filepath.Join(dir, filepath.FromSlash(rel))
		src, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		out, changed, err := rewriteImports(rel, src)
		if err != nil {
			return nil, err
		}
		if !changed {
			continue
		}
		if check {
			problems = append(problems, "imports "+upstreamModule+": "+rel)
			continue
		}
		if err := os.WriteFile(file, out, 0o644); err != nil {
			return nil, err
		}
	}

	// 3. Generate the facades.
	want := map[string][]byte{}
	for _, pkg := range exportPackages {
		src, err := generateExport(dir, pkg)
		if err != nil {
			return nil, err
		}
		want[path.Join(exportDir, pkg, exportFile)] = src
	}
	existing, err := listFiles(filepath.Join(dir, exportDir))
	if err != nil {
		return nil, err
	}
	for _, rel := range existing {
		rel = path.Join(exportDir, rel)
		if _, ok := want[rel]; ok || path.Base(rel) != exportFile {
			continue
		}
		if check {
			problems = append(problems, "stale facade: "+rel)
			continue
		}
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			return nil, err
		}
	}
	for _, rel := range sortedKeys(want) {
		file := filepath.Join(dir, filepath.FromSlash(rel))
		old, err := os.ReadFile(file)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if bytes.Equal(bytes.ReplaceAll(old, []byte("\r\n"), []byte("\n")), want[rel]) {
			continue
		}
		if check {
			problems = append(problems, "facade out of date: "+rel)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(file, want[rel], 0o644); err != nil {
			return nil, err
		}
	}
	return problems, nil
}

// unusedPaths returns the paths below dir (slash-separated, relative) that
// are not kept: whole top-level entries, internal packages and single files
// of kept packages.
func unusedPaths(dir string) ([]string, error) {
	var out []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case name == "internal" && e.IsDir():
		case name == exportDir && e.IsDir():
		case e.IsDir() && contains(keepDirs, name):
		case !e.IsDir() && contains(keepFiles, name):
		default:
			out = append(out, name)
		}
	}

	internal := filepath.Join(dir, "internal")
	err = filepath.WalkDir(internal, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == internal {
				return filepath.SkipDir
			}
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if p == internal {
				return nil
			}
			pkg := strings.TrimPrefix(rel, "internal/")
			if contains(keepPackages, pkg) {
				return nil
			}
			// A directory that contains a kept package is walked into;
			// anything else is removed as a whole.
			for _, k := range keepPackages {
				if strings.HasPrefix(k, pkg+"/") {
					return nil
				}
			}
			out = append(out, rel)
			return filepath.SkipDir
		}
		pkg := path.Dir(strings.TrimPrefix(rel, "internal/"))
		if !contains(keepPackages, pkg) || dropFiles.MatchString(rel) {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// rewriteImports replaces the upstream module path in the import specs of a
// Go file and formats the result (the changed paths can change the sort order
// within an import group).
func rewriteImports(name string, src []byte) ([]byte, bool, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly|parser.ParseComments)
	if err != nil {
		return nil, false, err
	}
	type edit struct{ start, end int }
	var edits []edit
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, "\"`")
		if p != upstreamModule && !strings.HasPrefix(p, upstreamModule+"/") {
			continue
		}
		start := fset.Position(imp.Path.Pos()).Offset
		end := fset.Position(imp.Path.End()).Offset
		edits = append(edits, edit{start, end})
	}
	if len(edits) == 0 {
		return src, false, nil
	}
	var b bytes.Buffer
	last := 0
	for _, e := range edits {
		b.Write(src[last:e.start])
		p := strings.Trim(string(src[e.start:e.end]), "\"`")
		fmt.Fprintf(&b, "%q", norcaPath+strings.TrimPrefix(p, upstreamModule))
		last = e.end
	}
	b.Write(src[last:])
	out, err := format.Source(b.Bytes())
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", name, err)
	}
	return out, true, nil
}

// decls are the exported top-level identifiers of a package.
type decls struct {
	types, consts, vars, funcs []string
}

// exportedDecls collects the exported top-level declarations of the non-test
// Go files of a package directory.
func exportedDecls(pkgDir string) (string, *decls, error) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return "", nil, err
	}
	seen := map[string]bool{}
	d := &decls{}
	pkgName := ""
	add := func(list *[]string, name string) {
		if ast.IsExported(name) && !seen[name] {
			seen[name] = true
			*list = append(*list, name)
		}
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(pkgDir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return "", nil, err
		}
		pkgName = f.Name.Name
		for _, decl := range f.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Recv != nil || !ast.IsExported(decl.Name.Name) {
					continue
				}
				if decl.Type.TypeParams != nil {
					return "", nil, fmt.Errorf("%s: generic function %s cannot be exported by reference; extend ubixsync", name, decl.Name.Name)
				}
				add(&d.funcs, decl.Name.Name)
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						if !ast.IsExported(spec.Name.Name) {
							continue
						}
						if spec.TypeParams != nil {
							return "", nil, fmt.Errorf("%s: generic type %s cannot be aliased; extend ubixsync", name, spec.Name.Name)
						}
						add(&d.types, spec.Name.Name)
					case *ast.ValueSpec:
						for _, n := range spec.Names {
							if !ast.IsExported(n.Name) {
								continue
							}
							if decl.Tok == token.CONST {
								add(&d.consts, n.Name)
								continue
							}
							// A copy of a variable would not see later
							// assignments; error values are never
							// reassigned, anything else needs a decision.
							if !strings.HasPrefix(n.Name, "Err") {
								return "", nil, fmt.Errorf("%s: exported variable %s is not an error value; decide how to export it", name, n.Name)
							}
							add(&d.vars, n.Name)
						}
					}
				}
			}
		}
	}
	if pkgName == "" {
		return "", nil, fmt.Errorf("%s: no Go package", pkgDir)
	}
	for _, l := range [][]string{d.types, d.consts, d.vars, d.funcs} {
		sort.Strings(l)
	}
	return pkgName, d, nil
}

// generateExport returns the facade of internal/<pkg>.
func generateExport(dir, pkg string) ([]byte, error) {
	name, d, err := exportedDecls(filepath.Join(dir, "internal", filepath.FromSlash(pkg)))
	if err != nil {
		return nil, err
	}
	imp := norcaPath + "/internal/" + pkg

	var b bytes.Buffer
	b.WriteString("// Code generated by nomad/tools/ubixsync. DO NOT EDIT.\n\n")
	b.WriteString("// SPDX-FileCopyrightText: 2026 Norca contributors\n")
	b.WriteString("// SPDX-License-Identifier: MPL-2.0\n\n")
	fmt.Fprintf(&b, "// Package %s exports the uBixVault package %s (BSD-3-Clause, see\n", name, pkg)
	fmt.Fprintf(&b, "// nomad/secretstore/ubix/LICENSE) to the rest of Norca. The original is a Go\n")
	fmt.Fprintf(&b, "// internal package of the imported subtree; this facade only refers to its\n")
	fmt.Fprintf(&b, "// exported identifiers: the types are aliases, so values pass freely between\n")
	fmt.Fprintf(&b, "// the two. The documentation is in the original package.\n")
	fmt.Fprintf(&b, "package %s\n\n", name)
	fmt.Fprintf(&b, "import ubix %q\n", imp)
	section := func(title, kw string, names []string, ref func(string) string) {
		if len(names) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n// %s\n%s (\n", title, kw)
		for _, n := range names {
			fmt.Fprintf(&b, "\t%s\n", ref(n))
		}
		b.WriteString(")\n")
	}
	section("Types.", "type", d.types, func(n string) string { return n + " = ubix." + n })
	section("Constants.", "const", d.consts, func(n string) string { return n + " = ubix." + n })
	section("Errors.", "var", d.vars, func(n string) string { return n + " = ubix." + n })
	section("Functions.", "var", d.funcs, func(n string) string { return n + " = ubix." + n })
	return format.Source(b.Bytes())
}

// listGoFiles returns the Go files below dir, slash-separated and relative.
func listGoFiles(dir string) ([]string, error) {
	all, err := listFiles(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range all {
		if strings.HasSuffix(f, ".go") {
			out = append(out, f)
		}
	}
	return out, nil
}

// listFiles returns the files below dir, slash-separated and relative; none
// if dir does not exist.
func listFiles(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == dir {
				return filepath.SkipDir
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out, err
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

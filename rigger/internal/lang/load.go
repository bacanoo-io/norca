// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package lang

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/rigger/internal/syntax"
)

// Limits bound what Load reads (design 12.4).
type Limits struct {
	// ModuleBytes bounds the size of all files of a module.
	ModuleBytes int64
	// FileBytes bounds the size of one file.
	FileBytes int64
	// Depth bounds the nesting of child modules; 0 is no bound.
	Depth int
	// Modules bounds the modules of one load, each counted as often as it
	// is called; 0 is no bound.
	Modules int

	// loaded counts over one load: the limits are passed down by value.
	loaded *loaded
}

// parsedFactor is how many times ModuleBytes the parsed files of all
// modules of one load may have.
const parsedFactor = 4

// loaded is what one load has read.
type loaded struct {
	modules int
	bytes   int64
	// over is set once a bound is passed: the loops of the load stop.
	over bool
}

// start returns the limits of a new load.
func (l Limits) start() Limits {
	l.loaded = &loaded{}

	return l
}

// takeModule counts one more module, and reports whether it is within the
// bound.
func (l Limits) takeModule() bool {
	l.loaded.modules++
	if l.Modules > 0 && l.loaded.modules > l.Modules {
		l.loaded.over = true
	}

	return !l.loaded.over
}

// takeBytes counts n more parsed bytes, and reports whether they are within
// the bound.
func (l Limits) takeBytes(n int) bool {
	l.loaded.bytes += int64(n)
	if l.ModuleBytes > 0 && l.loaded.bytes > parsedFactor*l.ModuleBytes {
		l.loaded.over = true
	}

	return !l.loaded.over
}

func tooManyModules(limit int) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagError, Summary: "Too many modules",
		Detail: fmt.Sprintf("They are more than %d modules with their child modules, the limit of the engine.", limit),
	}
}

// topDirs are the directories a module may have.
var topDirs = map[string]bool{"files": true, "examples": true, "tests": true}

// The schemas of the files that hold blocks of one kind or two.
var (
	localsSchema = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{
		{Type: "locals"}, {Type: "check", LabelNames: []string{labelName}},
	}}
	outputsSchema = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{
		{Type: "output", LabelNames: []string{labelName}},
	}}
	modulesSchema = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{
		{Type: "module", LabelNames: []string{labelName}},
	}}
)

// loader reads one module.
type loader struct {
	ctx    context.Context //nolint:containedctx // lives for one call of Load
	fsys   fs.FS
	limits Limits
	// prefix is the directory of the module from the module loaded first,
	// which names its files; empty for that module.
	prefix string
	total  int64
	mod    *Module
	diags  hcl.Diagnostics
}

// Load reads the module in fsys, whose root is the directory of the module.
// It reads nothing outside fsys, and stops when ctx is cancelled.
func Load(ctx context.Context, fsys fs.FS, limits Limits) (*Module, hcl.Diagnostics) {
	return LoadAt(ctx, fsys, ".", limits)
}

// LoadAt reads the module in the directory dir of root, and its child
// modules. A child module comes from a local source, a directory relative
// to the module that calls it, inside root: nothing outside root is read.
func LoadAt(ctx context.Context, root fs.FS, dir string, limits Limits) (*Module, hcl.Diagnostics) {
	return loadTree(ctx, root, path.Clean(dir), "", limits.start(), nil)
}

// loadTree loads the module in dir and its children; stack holds the
// directories of the modules that call it.
func loadTree(ctx context.Context, root fs.FS, dir, prefix string, limits Limits, stack []string) (*Module, hcl.Diagnostics) {
	if !limits.takeModule() {
		return &Module{Sources: map[string][]byte{}}, hcl.Diagnostics{tooManyModules(limits.Modules)}
	}

	fsys, err := fs.Sub(root, dir)
	if err != nil {
		return &Module{Sources: map[string][]byte{}}, hcl.Diagnostics{{
			Severity: hcl.DiagError, Summary: "Module not read", Detail: err.Error() + ".",
		}}
	}

	l := &loader{ctx: ctx, fsys: fsys, limits: limits, prefix: prefix, mod: &Module{Sources: map[string][]byte{}, FS: fsys, Root: root, Dir: dir}}
	m, diags := l.load()

	if m.Metadata.Library && len(m.Resources) > 0 {
		diags = append(diags, errorAt(m.Resources[0].Block.DefRange, "Resource in a library module",
			"A module with library = true has no resources: it only computes values for the modules that call it."))
	}

	if diags.HasErrors() {
		return m, diags
	}

	return m, append(diags, loadChildren(ctx, root, m, prefix, limits, append(stack, dir))...)
}

// load reads the files of the module.
func (l *loader) load() (*Module, hcl.Diagnostics) {
	err := fs.WalkDir(l.fsys, ".", l.visit)
	if err != nil {
		l.diags = append(l.diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Module not read",
			Detail:   err.Error() + ".",
		})

		return l.mod, l.diags
	}

	if _, ok := l.mod.Sources[l.name("metadata.hcl")]; !ok {
		l.diags = append(l.diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Missing metadata.hcl",
			Detail:   "A module has a metadata.hcl with its module block.",
		})
	}

	return l.mod, l.diags
}

// name is the name of the file at p of the module in diagnostics and in
// Sources.
func (l *loader) name(p string) string {
	if l.prefix == "" {
		return p
	}

	return path.Join(l.prefix, p)
}

func (l *loader) visit(p string, d fs.DirEntry, err error) error {
	if err != nil {
		return err
	}

	if err := l.ctx.Err(); err != nil {
		return err
	}

	if p != "." && !isHidden(d.Name()) {
		if diag := nameDiag(d.Name()); diag != nil {
			l.diags = append(l.diags, diag)

			if d.IsDir() {
				return fs.SkipDir
			}

			return nil
		}
	}

	if d.IsDir() {
		return l.visitDir(p, d)
	}

	kind, rt, name := Classify(p)
	if kind == FileUnknown {
		l.diags = append(l.diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Unknown file",
			Detail: fmt.Sprintf("%s is not a file a module may have: a file is named after what it holds, "+
				"plain files go under files/.", l.name(p)),
		})

		return nil
	}

	if err := l.count(p, d); err != nil {
		return err
	}

	return l.parse(p, kind, rt, name)
}

func (l *loader) visitDir(p string, d fs.DirEntry) error {
	switch {
	case p == ".":
		return nil
	case strings.HasPrefix(d.Name(), "."):
		return fs.SkipDir
	case strings.Count(p, "/") == 0 && topDirs[p]:
		return nil
	case strings.HasPrefix(p, "files/"):
		return nil
	}

	l.diags = append(l.diags, &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  "Unknown directory",
		Detail:   l.name(p) + " is not a directory a module may have: files/, examples/ and tests/.",
	})

	return fs.SkipDir
}

// count adds the size of the file at p to the total and checks the limits.
func (l *loader) count(p string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return err
	}

	size := info.Size()
	l.total += size

	switch {
	case size > l.limits.FileBytes:
		l.diags = append(l.diags, limitDiag(fmt.Sprintf("%s has %d bytes, more than the %d a file may have.",
			l.name(p), size, l.limits.FileBytes)))
	case l.total > l.limits.ModuleBytes:
		l.diags = append(l.diags, limitDiag(fmt.Sprintf("The files of the module have more than the %d bytes a module may have.",
			l.limits.ModuleBytes)))
	default:
		return nil
	}

	return errLimitStop
}

// errLimitStop ends the walk after a limit diagnostic.
var errLimitStop = fs.SkipAll

func limitDiag(detail string) *hcl.Diagnostic {
	return &hcl.Diagnostic{Severity: hcl.DiagError, Summary: "Module too large", Detail: detail}
}

// parse reads, parses and decodes a file of the module.
func (l *loader) parse(p string, kind FileKind, rt ResourceType, name string) error {
	switch kind {
	case FileExample:
		l.mod.Examples = append(l.mod.Examples, p)

		return nil
	case FileTest:
		l.mod.Tests = append(l.mod.Tests, p)

		return nil
	case FileData, FileDoc, FileUnknown:
		return nil
	case FileMetadata, FileVariables, FileLocals, FileModules, FileOutputs, FileResource:
	}

	src, err := fs.ReadFile(l.fsys, p)
	if err != nil {
		return err
	}

	if !l.limits.takeBytes(len(src)) {
		l.diags = append(l.diags, &hcl.Diagnostic{
			Severity: hcl.DiagError, Summary: "Modules too large",
			Detail: fmt.Sprintf("The files of the modules together have more than %d bytes to parse, the limit of the engine.",
				parsedFactor*l.limits.ModuleBytes),
		})

		return errLimitStop
	}

	l.mod.Sources[l.name(p)] = src

	file, diags := syntax.ParseConfig(src, l.name(p))
	l.diags = append(l.diags, diags...)

	if diags.HasErrors() {
		return nil
	}

	l.diags = append(l.diags, l.decode(file, kind, rt, name)...)

	return nil
}

func (l *loader) decode(file *hcl.File, kind FileKind, rt ResourceType, name string) hcl.Diagnostics {
	m := l.mod

	switch kind {
	case FileMetadata:
		return m.decodeMetadata(file)
	case FileVariables:
		return m.decodeVariables(file.Body)
	case FileLocals:
		content, diags := file.Body.Content(localsSchema)
		for _, b := range content.Blocks {
			if b.Type == "check" {
				m.Checks = append(m.Checks, b)
			} else {
				m.Locals = append(m.Locals, b)
			}
		}

		return diags
	case FileOutputs:
		content, diags := file.Body.Content(outputsSchema)
		m.Outputs = append(m.Outputs, content.Blocks...)

		return diags
	case FileModules:
		content, diags := file.Body.Content(modulesSchema)
		m.Calls = append(m.Calls, content.Blocks...)

		return diags
	case FileResource:
		return m.decodeResource(file, rt, name)
	case FileUnknown, FileData, FileExample, FileTest, FileDoc:
	}

	return nil
}

// decodeResource checks that a resource file holds one block of its kind,
// labelled with the name of the file (design 4.5).
func (m *Module) decodeResource(file *hcl.File, rt ResourceType, name string) hcl.Diagnostics {
	content, diags := file.Body.Content(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{{Type: rt.Block, LabelNames: []string{labelName}}},
	})
	if diags.HasErrors() {
		return diags
	}

	if len(content.Blocks) != 1 {
		return append(diags, errorAt(file.Body.MissingItemRange(), "One resource per file",
			fmt.Sprintf("A %s file holds one %s block.", rt.Kind, rt.Block)))
	}

	b := content.Blocks[0]
	if b.Labels[0] != name {
		return append(diags, errorAt(b.LabelRanges[0], "Label and file name differ",
			fmt.Sprintf("The %s block of this file is labelled %q; the file says %q.", rt.Block, b.Labels[0], name)))
	}

	m.Resources = append(m.Resources, &Resource{Type: rt, Name: name, Block: b})

	return diags
}

// LoadAs reads the module in the directory dir of root, as LoadAt does,
// naming its files with prefix in diagnostics: a module fetched from a
// source, named after its address and version.
func LoadAs(ctx context.Context, root fs.FS, dir, prefix string, limits Limits) (*Module, hcl.Diagnostics) {
	return loadTree(ctx, root, path.Clean(dir), prefix, limits.start(), nil)
}

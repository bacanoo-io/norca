// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package lang

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// Stack is a loaded stack (design 6.4): the modules of a region with their
// values, as releases.
type Stack struct {
	Rig RigBlock
	// Vars holds the variables and rules of variables.hcl, in the shape of
	// a module, so that the values of a stack are checked as a module's.
	Vars *Module
	// Locals, Checks and Outputs are the blocks of locals.hcl and
	// outputs.hcl.
	Locals  []*hcl.Block
	Checks  []*hcl.Block
	Outputs []*hcl.Block
	// Releases are the module blocks of stack.hcl, in the order written.
	Releases []*StackRelease
	// Secrets are the secret blocks of secrets.hcl.
	Secrets []*hcl.Block
	// ValueFiles are the paths of the value files under values/.
	ValueFiles []string
	Sources    map[string][]byte
	FS         fs.FS
	Dir        string
}

// RigBlock is the rig block of stack.hcl.
type RigBlock struct {
	RequiredVersion string `hcl:"required_version,optional"`
	// Name is the name of the stack, the owner of its releases.
	Name   string `hcl:"name"`
	Region string `hcl:"region,optional"`
	// Namespace is the namespace of a release that names none.
	Namespace string `hcl:"namespace,optional"`
	// Registries and Trust are read in phases 3 and 4.
	Registries []*hcl.Block `hcl:"registry,block"`
	Trust      []*hcl.Block `hcl:"trust,block"`
	Range      hcl.Range
}

// StackRelease is a module block of a stack: a release.
type StackRelease struct {
	// Name is the label: the name of the release.
	Name    string
	Source  string
	Version string
	// Namespace, Values and Enabled are expressions over var.*, local.*
	// and module.*; nil when they are not written.
	Namespace hcl.Expression
	Values    hcl.Expression
	Enabled   hcl.Expression
	// DependsOn are the names of the releases written in depends_on.
	DependsOn []string
	// The settings of the release (design 6.4). Wait is false only when
	// written so: a stack waits until a release is ready.
	Wait       *bool
	Atomic     *bool
	Timeout    time.Duration
	MaxHistory int
	Block      *hcl.Block
	// Module is the module of a local source, loaded with the stack.
	Module *Module
}

// The attributes of a module block of a stack that are expressions.
const (
	attrNamespace = "namespace"
	attrValues    = "values"
)

// releaseName is the name of a release, the rule of the deployer.
var releaseName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// stackDirs are the directories a stack may have.
var stackDirs = map[string]bool{"modules": true, "values": true, "keys": true, "examples": true, "files": true}

var (
	stackSchema = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{
		{Type: "rig"}, {Type: "module", LabelNames: []string{labelName}},
	}}
	secretsSchema = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{
		{Type: "secret", LabelNames: []string{labelName}},
	}}
)

// The attributes of a module block of a stack.
var releaseAttrs = map[string]bool{
	attrSource: true, attrVersion: true, attrNamespace: true, attrValues: true, "enabled": true,
	"depends_on": true, "atomic": true, "timeout": true, "max_history": true, "wait": true,
}

// LoadStack reads the stack in the directory dir of root, and the modules
// of its local sources (./modules/x, relative to the stack, inside root).
// A source of a registry is refused until phase 3.
func LoadStack(ctx context.Context, root fs.FS, dir string, limits Limits) (*Stack, hcl.Diagnostics) {
	dir = path.Clean(dir)

	fsys, err := fs.Sub(root, dir)
	if err != nil {
		return nil, hcl.Diagnostics{{Severity: hcl.DiagError, Summary: "Stack not read", Detail: err.Error() + "."}}
	}

	s := &Stack{Sources: map[string][]byte{}, FS: fsys, Dir: dir, Vars: &Module{Sources: map[string][]byte{}}}

	diags := s.readFiles(ctx, fsys)
	if diags.HasErrors() {
		return s, diags
	}

	if _, ok := s.Sources["stack.hcl"]; !ok {
		return s, append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError, Summary: "Missing stack.hcl",
			Detail: "A stack has a stack.hcl with its rig block and its modules (design 6.4).",
		})
	}

	s.Vars.Metadata.Name = s.Rig.Name

	for _, r := range s.Releases {
		diags = append(diags, s.loadRelease(ctx, root, r, limits)...)
	}

	return s, diags
}

// readFiles reads and decodes the files of the stack.
func (s *Stack) readFiles(ctx context.Context, fsys fs.FS) hcl.Diagnostics {
	var diags hcl.Diagnostics

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case ctx.Err() != nil:
			return ctx.Err()
		case p == ".":
			return nil
		case strings.HasPrefix(d.Name(), "."):
			if d.IsDir() {
				return fs.SkipDir
			}

			return nil
		case d.IsDir():
			if stackDirs[p] {
				if p == "values" {
					return nil
				}

				return fs.SkipDir
			}

			if strings.HasPrefix(p, "values/") {
				return nil
			}

			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Unknown directory",
				Detail: p + " is not a directory a stack may have: modules/, values/, keys/, examples/, files/ (design 7.5).",
			})

			return fs.SkipDir
		}

		diags = append(diags, s.readFile(fsys, p)...)

		return nil
	})
	if err != nil {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: "Stack not read", Detail: err.Error() + "."})
	}

	return diags
}

// readFile reads one file of the stack by its name.
func (s *Stack) readFile(fsys fs.FS, p string) hcl.Diagnostics {
	switch {
	case strings.HasPrefix(p, "values/"):
		if strings.HasSuffix(p, ".vars.hcl") {
			s.ValueFiles = append(s.ValueFiles, p)
		}

		return nil
	case !strings.HasSuffix(p, ".hcl") || p == "rig.lock.hcl":
		return nil // documentation, the icon; the lock file is read in phase 3
	}

	src, err := fs.ReadFile(fsys, p)
	if err != nil {
		return hcl.Diagnostics{{Severity: hcl.DiagError, Summary: "File not read", Detail: err.Error() + "."}}
	}

	s.Sources[p] = src

	file, diags := hclsyntax.ParseConfig(src, p, hcl.InitialPos)
	if diags.HasErrors() {
		return diags
	}

	return append(diags, s.decodeFile(p, src, file)...)
}

// decodeFile decodes a file of the stack by its name.
func (s *Stack) decodeFile(p string, src []byte, file *hcl.File) hcl.Diagnostics {
	var diags hcl.Diagnostics

	switch p {
	case "stack.hcl":
		return s.decodeStack(file)
	case "variables.hcl":
		s.Vars.Sources[p] = src

		return s.Vars.decodeVariables(file.Body)
	case "locals.hcl":
		content, moreDiags := file.Body.Content(localsSchema)
		for _, b := range content.Blocks {
			if b.Type == "check" {
				s.Checks = append(s.Checks, b)
			} else {
				s.Locals = append(s.Locals, b)
			}
		}

		return append(diags, moreDiags...)
	case "outputs.hcl":
		content, moreDiags := file.Body.Content(outputsSchema)
		s.Outputs = append(s.Outputs, content.Blocks...)

		return append(diags, moreDiags...)
	case "secrets.hcl":
		content, moreDiags := file.Body.Content(secretsSchema)
		s.Secrets = append(s.Secrets, content.Blocks...)

		return append(diags, moreDiags...)
	case "metadata.hcl":
		return nil // the stack block of a published stack, read in phase 3
	default:
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError, Summary: "Unknown file",
			Detail: p + " is not a file a stack may have: stack.hcl, variables.hcl, locals.hcl, outputs.hcl, secrets.hcl, metadata.hcl, rig.lock.hcl (design 7.5).",
		})
	}
}

// decodeStack decodes stack.hcl: one rig block and the module blocks.
func (s *Stack) decodeStack(file *hcl.File) hcl.Diagnostics {
	content, diags := file.Body.Content(stackSchema)

	var rigs int

	for _, b := range content.Blocks {
		switch b.Type {
		case "rig":
			rigs++

			diags = append(diags, gohcl.DecodeBody(b.Body, nil, &s.Rig)...)
			s.Rig.Range = b.DefRange
		default:
			r, moreDiags := decodeRelease(b)
			diags = append(diags, moreDiags...)

			if r != nil {
				s.Releases = append(s.Releases, r)
			}
		}
	}

	if rigs != 1 {
		diags = append(diags, errorAt(file.Body.MissingItemRange(), "One rig block",
			"stack.hcl has one rig block with the name of the stack (design 6.4)."))
	}

	if s.Rig.Name != "" && !releaseName.MatchString(s.Rig.Name) {
		diags = append(diags, errorAt(s.Rig.Range, "Invalid stack name",
			fmt.Sprintf("%q is not a name of a stack: lower case letters, digits and -, at most 63.", s.Rig.Name)))
	}

	return diags
}

// decodeRelease decodes a module block of a stack.
func decodeRelease(b *hcl.Block) (*StackRelease, hcl.Diagnostics) {
	name := b.Labels[0]
	if !releaseName.MatchString(name) {
		return nil, hcl.Diagnostics{errorAt(b.LabelRanges[0], "Invalid release name",
			fmt.Sprintf("%q is not a name of a release: lower case letters, digits and -, at most 63 (design 6.4).", name))}
	}

	attrs, diags := b.Body.JustAttributes()
	if diags.HasErrors() {
		return nil, diags
	}

	r := &StackRelease{Name: name, Block: b}

	for n, attr := range attrs {
		if !releaseAttrs[n] {
			diags = append(diags, errorAt(attr.NameRange, "Unknown attribute",
				fmt.Sprintf("A module of a stack takes source, version, namespace, values, enabled, depends_on, atomic, wait, timeout and max_history, not %s.", n)))

			continue
		}

		diags = append(diags, r.decodeAttr(n, attr)...)
	}

	if r.Source == "" && !diags.HasErrors() {
		diags = append(diags, errorAt(b.DefRange, "Missing source", "A module of a stack names its source."))
	}

	return r, diags
}

// decodeAttr decodes one attribute of a module block of a stack.
func (r *StackRelease) decodeAttr(name string, attr *hcl.Attribute) hcl.Diagnostics {
	switch name {
	case attrNamespace:
		r.Namespace = attr.Expr
	case attrValues:
		r.Values = attr.Expr
	case "enabled":
		r.Enabled = attr.Expr
	case "depends_on":
		return r.decodeDependsOn(attr)
	default:
		return r.decodeConstant(name, attr)
	}

	return nil
}

// decodeConstant decodes an attribute that is a constant.
func (r *StackRelease) decodeConstant(name string, attr *hcl.Attribute) hcl.Diagnostics {
	var diags hcl.Diagnostics

	switch name {
	case attrSource:
		diags = gohcl.DecodeExpression(attr.Expr, nil, &r.Source)
	case attrVersion:
		diags = gohcl.DecodeExpression(attr.Expr, nil, &r.Version)
	case "atomic":
		var atomic bool

		diags = gohcl.DecodeExpression(attr.Expr, nil, &atomic)
		r.Atomic = &atomic
	case "wait":
		var wait bool

		diags = gohcl.DecodeExpression(attr.Expr, nil, &wait)
		r.Wait = &wait
	case "max_history":
		diags = gohcl.DecodeExpression(attr.Expr, nil, &r.MaxHistory)
	case "timeout":
		var timeout string
		if diags = gohcl.DecodeExpression(attr.Expr, nil, &timeout); diags.HasErrors() {
			return diags
		}

		d, err := time.ParseDuration(timeout)
		if err != nil || d <= 0 {
			return hcl.Diagnostics{errorAt(attr.Expr.Range(), "Invalid timeout",
				fmt.Sprintf("%q is not a duration such as \"10m\".", timeout))}
		}

		r.Timeout = d
	}

	return diags
}

// decodeDependsOn decodes depends_on: a list of module.<name>.
func (r *StackRelease) decodeDependsOn(attr *hcl.Attribute) hcl.Diagnostics {
	exprs, diags := hcl.ExprList(attr.Expr)
	if diags.HasErrors() {
		return diags
	}

	for _, e := range exprs {
		t, moreDiags := hcl.AbsTraversalForExpr(e)
		if moreDiags.HasErrors() || len(t) != 2 || t.RootName() != "module" {
			diags = append(diags, errorAt(e.Range(), "Invalid depends_on",
				"depends_on lists modules of the stack: [module.postgres, module.nats]."))

			continue
		}

		attr, ok := t[1].(hcl.TraverseAttr)
		if !ok {
			diags = append(diags, errorAt(e.Range(), "Invalid depends_on", "depends_on lists modules of the stack: [module.postgres, module.nats]."))

			continue
		}

		r.DependsOn = append(r.DependsOn, attr.Name)
	}

	return diags
}

// loadRelease loads the module of a release with a local source.
func (s *Stack) loadRelease(ctx context.Context, root fs.FS, r *StackRelease, limits Limits) hcl.Diagnostics {
	rng := r.Block.DefRange

	switch {
	case strings.HasPrefix(r.Source, "./") || strings.HasPrefix(r.Source, "../"):
	default:
		return hcl.Diagnostics{errorAt(rng, "Source not supported yet",
			r.Source+" is not a local source (./ or ../). Modules from registries come with phase 3.")}
	}

	if r.Version != "" {
		return hcl.Diagnostics{errorAt(rng, "Version of a local source",
			"A local module is part of the stack and versioned with it; it has no version (design 6.4).")}
	}

	dir := path.Join(s.Dir, r.Source)
	if dir == ".." || strings.HasPrefix(dir, "../") {
		return hcl.Diagnostics{errorAt(rng, "Source outside the root",
			r.Source+" is outside the files the stack was loaded from.")}
	}

	m, diags := loadTree(ctx, root, dir, r.Source, limits, nil)
	r.Module = m

	if m.Metadata.Library {
		diags = append(diags, errorAt(rng, "Library module",
			fmt.Sprintf("The module %q is a library; another module calls it, a stack does not install it.", m.Metadata.Name)))
	}

	return diags
}

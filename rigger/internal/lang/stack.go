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

	"github.com/bacanoo-io/norca/rigger/internal/syntax"
	"github.com/bacanoo-io/norca/rigger/module"
	"github.com/bacanoo-io/norca/rigger/sign"
	"github.com/bacanoo-io/norca/rigger/source"
)

// Stack is a loaded stack (design 6.4): the modules of a region with their
// values, as releases.
type Stack struct {
	Rigger RiggerBlock
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
	// Parts are the stack blocks of stack.hcl: published stacks taken as
	// parts (design 7.5).
	Parts []*StackPart
	// Secrets are the secret blocks of secrets.hcl.
	Secrets []*hcl.Block
	// ValueFiles are the paths of the value files under values/.
	ValueFiles []string
	// Metadata is the stack block of metadata.hcl of a published stack
	// (design 7.5); nil for a stack that has none.
	Metadata *module.Metadata
	// Lock is the lock file of the stack; nil without one.
	Lock    *Lock
	Sources map[string][]byte
	FS      fs.FS
	Dir     string
	// Root is what the stack was loaded from; FS is Dir of it.
	Root fs.FS
}

// RiggerBlock is the rigger block of stack.hcl.
type RiggerBlock struct {
	RequiredVersion string `hcl:"required_version,optional"`
	// Name is the name of the stack, the owner of its releases.
	Name   string `hcl:"name"`
	Region string `hcl:"region,optional"`
	// Namespace is the namespace of a release that names none.
	Namespace string `hcl:"namespace,optional"`
	// Registries are the registry blocks; Trust the trust blocks, read in
	// phase 4.
	Registries []*hcl.Block
	Trust      []*hcl.Block
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
	// DependsOn are the names of the releases written in depends_on,
	// DependsOnParts those of the parts.
	DependsOn      []string
	DependsOnParts []string
	// The settings of the release (design 6.4). Wait is false only when
	// written so: a stack waits until a release is ready.
	Wait       *bool
	Atomic     *bool
	Timeout    time.Duration
	MaxHistory int
	Block      *hcl.Block
	// Module is the module of the release: of a local source loaded with
	// the stack, of another source loaded by the engine from the lock.
	Module *Module
	// Address is the source of a module that is not local; nil for a local
	// one. Locked is its entry of the lock file, set by the engine.
	Address *source.Request
	Locked  *LockedModule
	// Signatures are the signatures of the module of a source, Signers the
	// keys of them the trust of the stack took.
	Signatures []sign.Signature
	Signers    []string
}

// StackPart is a stack block of a stack: a published stack of a registry
// taken as a part (design 7.5). Its releases are named <part>-<release>;
// the modules of the stack read its outputs as stack.<part>.<output>. A
// part has no parts of its own.
type StackPart struct {
	// Name is the label.
	Name    string
	Source  string
	Version string
	// Values are the values of the variables of the part, an expression
	// over var.*, local.*, module.* and stack.*; nil when not written.
	Values hcl.Expression
	// DependsOn and DependsOnParts are the releases and parts written in
	// depends_on.
	DependsOn      []string
	DependsOnParts []string
	Block          *hcl.Block
	Address        *source.Request
	// Stack is the published stack, Locked its entry of the lock file,
	// both set by the engine.
	Stack  *Stack
	Locked *LockedModule
	// Signatures and Signers are those of the published stack.
	Signatures []sign.Signature
	Signers    []string
}

// blockStack is the stack block of a stack, a part, and the root of the
// references to its outputs.
const blockStack = "stack"

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
		{Type: "rigger"}, {Type: "module", LabelNames: []string{labelName}}, {Type: blockStack, LabelNames: []string{labelName}},
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

	s := &Stack{Sources: map[string][]byte{}, FS: fsys, Dir: dir, Root: root, Vars: &Module{Sources: map[string][]byte{}}}

	diags := s.readFiles(ctx, fsys, limits)
	if diags.HasErrors() {
		return s, diags
	}

	if _, ok := s.Sources["stack.hcl"]; !ok {
		return s, append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError, Summary: "Missing stack.hcl",
			Detail: "A stack has a stack.hcl with its rigger block and its modules (design 6.4).",
		})
	}

	s.Vars.Metadata.Name = s.Rigger.Name

	// Before any is loaded or fetched: the modules of sources come later,
	// one fetch for each.
	if limits.Modules > 0 && len(s.Releases)+len(s.Parts) > limits.Modules {
		return s, append(diags, tooManyModules(limits.Modules))
	}

	limits = limits.start()

	for _, r := range s.Releases {
		diags = append(diags, s.loadRelease(ctx, root, r, limits)...)

		if limits.loaded.over {
			return s, diags
		}
	}

	for _, p := range s.Parts {
		diags = append(diags, s.parsePart(p)...)
	}

	if s.Metadata != nil && !diags.HasErrors() {
		diags = append(diags, s.checkPublished()...)
	}

	return s, diags
}

// checkPublished checks the rules of a published stack (design 7.5): the
// name of its metadata is that of its rigger block, and its own modules and
// their children lie in its directory, so that its digest covers them.
func (s *Stack) checkPublished() hcl.Diagnostics {
	var diags hcl.Diagnostics

	for _, p := range s.Parts {
		diags = append(diags, errorAt(p.Block.DefRange, "Part of a published stack",
			"A published stack has no stack blocks: parts are one level, a local stack over published stacks (design 7.5)."))
	}

	if s.Metadata.Name != s.Rigger.Name {
		diags = append(diags, errorAt(s.Rigger.Range, "Names differ",
			fmt.Sprintf("metadata.hcl names the stack %q, the rigger block %q.", s.Metadata.Name, s.Rigger.Name)))
	}

	for _, r := range s.Releases {
		if r.Module == nil || r.Address != nil {
			continue
		}

		for _, dir := range treeDirs(r.Module) {
			if !s.inModules(dir) {
				diags = append(diags, errorAt(r.Block.DefRange, "Module outside the stack",
					dir+" is not in modules/ of the stack: a published stack carries its own modules (design 7.5)."))

				break
			}
		}
	}

	return diags
}

// inModules reports whether a directory of the root lies in modules/ of the
// stack.
func (s *Stack) inModules(dir string) bool {
	rel := path.Clean(dir)

	if s.Dir != "." {
		var ok bool
		if rel, ok = strings.CutPrefix(rel, s.Dir+"/"); !ok {
			return false
		}
	}

	return strings.HasPrefix(rel, "modules/")
}

// treeDirs are the directories of a module and of its local children.
func treeDirs(m *Module) []string {
	dirs := []string{m.Dir}

	for _, c := range m.Children {
		dirs = append(dirs, treeDirs(c.Module)...)
	}

	return dirs
}

// readFiles reads and decodes the files of the stack.
func (s *Stack) readFiles(ctx context.Context, fsys fs.FS, limits Limits) hcl.Diagnostics {
	var diags hcl.Diagnostics

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case ctx.Err() != nil:
			return ctx.Err()
		case p == ".":
			return nil
		case isHidden(d.Name()):
			if d.IsDir() {
				return fs.SkipDir
			}

			return nil
		case nameDiag(d.Name()) != nil:
			diags = append(diags, nameDiag(d.Name()))

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

		diags = append(diags, s.readFile(fsys, p, limits)...)

		return nil
	})
	if err != nil {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: "Stack not read", Detail: err.Error() + "."})
	}

	return diags
}

// readFile reads one file of the stack by its name.
func (s *Stack) readFile(fsys fs.FS, p string, limits Limits) hcl.Diagnostics {
	switch {
	case strings.HasPrefix(p, "values/"):
		if strings.HasSuffix(p, ".vars.hcl") {
			s.ValueFiles = append(s.ValueFiles, p)
		}

		return nil
	case !strings.HasSuffix(p, ".hcl"):
		return nil // documentation, the icon
	}

	if limits.FileBytes > 0 {
		info, err := fs.Stat(fsys, p)
		if err != nil {
			return hcl.Diagnostics{{Severity: hcl.DiagError, Summary: "File not read", Detail: err.Error() + "."}}
		}

		if info.Size() > limits.FileBytes {
			return hcl.Diagnostics{{
				Severity: hcl.DiagError, Summary: "File too large",
				Detail: fmt.Sprintf("%s has %d bytes, more than the %d a file of a stack may have.", p, info.Size(), limits.FileBytes),
			}}
		}
	}

	src, err := fs.ReadFile(fsys, p)
	if err != nil {
		return hcl.Diagnostics{{Severity: hcl.DiagError, Summary: "File not read", Detail: err.Error() + "."}}
	}

	s.Sources[p] = src

	file, diags := syntax.ParseConfig(src, p)
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
	case LockFile:
		lock := &Lock{}
		diags = gohcl.DecodeBody(file.Body, nil, lock)
		s.Lock = lock

		return diags
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
		return s.decodeMetadata(file)
	default:
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError, Summary: "Unknown file",
			Detail: p + " is not a file a stack may have: stack.hcl, variables.hcl, locals.hcl, outputs.hcl, secrets.hcl, metadata.hcl, " + LockFile + " (design 7.5).",
		})
	}
}

// decodeStack decodes stack.hcl: one rigger block and the module blocks.
func (s *Stack) decodeStack(file *hcl.File) hcl.Diagnostics {
	content, diags := file.Body.Content(stackSchema)

	var riggers int

	for _, b := range content.Blocks {
		switch b.Type {
		case "rigger":
			riggers++

			diags = append(diags, s.decodeRigger(b)...)
		case blockStack:
			p, moreDiags := decodePart(b)
			diags = append(diags, moreDiags...)

			if p != nil {
				s.Parts = append(s.Parts, p)
			}
		default:
			r, moreDiags := decodeRelease(b)
			diags = append(diags, moreDiags...)

			if r != nil {
				s.Releases = append(s.Releases, r)
			}
		}
	}

	if riggers != 1 {
		diags = append(diags, errorAt(file.Body.MissingItemRange(), "One rigger block",
			"stack.hcl has one rigger block with the name of the stack (design 6.4)."))
	}

	if s.Rigger.Name != "" && !releaseName.MatchString(s.Rigger.Name) {
		diags = append(diags, errorAt(s.Rigger.Range, "Invalid stack name",
			fmt.Sprintf("%q is not a name of a stack: lower case letters, digits and -, at most 63.", s.Rigger.Name)))
	}

	return diags
}

// riggerSchema is the schema of the blocks of the rigger block.
var riggerSchema = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{
	{Type: "registry", LabelNames: []string{labelName}}, {Type: "trust"},
}}

// decodeRigger decodes the rigger block: its attributes, its registries and its
// trust.
func (s *Stack) decodeRigger(b *hcl.Block) hcl.Diagnostics {
	content, remain, diags := b.Body.PartialContent(riggerSchema)

	for _, inner := range content.Blocks {
		if inner.Type == "registry" {
			s.Rigger.Registries = append(s.Rigger.Registries, inner)
		} else {
			s.Rigger.Trust = append(s.Rigger.Trust, inner)
		}
	}

	s.Rigger.Range = b.DefRange

	return append(diags, gohcl.DecodeBody(remain, nil, &s.Rigger)...)
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

// decodeDependsOn decodes depends_on: a list of module.<name> and
// stack.<name>.
func (r *StackRelease) decodeDependsOn(attr *hcl.Attribute) hcl.Diagnostics {
	var diags hcl.Diagnostics

	r.DependsOn, r.DependsOnParts, diags = decodeDependsOn(attr)

	return diags
}

// decodeDependsOn decodes depends_on: the names of the modules and of the
// parts it lists.
func decodeDependsOn(attr *hcl.Attribute) (modules, parts []string, diags hcl.Diagnostics) {
	exprs, diags := hcl.ExprList(attr.Expr)
	if diags.HasErrors() {
		return nil, nil, diags
	}

	const detail = "depends_on lists modules and parts of the stack: [module.postgres, stack.data]."

	for _, e := range exprs {
		t, moreDiags := hcl.AbsTraversalForExpr(e)
		if moreDiags.HasErrors() || len(t) != 2 || (t.RootName() != "module" && t.RootName() != blockStack) {
			diags = append(diags, errorAt(e.Range(), "Invalid depends_on", detail))

			continue
		}

		attr, ok := t[1].(hcl.TraverseAttr)
		if !ok {
			diags = append(diags, errorAt(e.Range(), "Invalid depends_on", detail))

			continue
		}

		if t.RootName() == blockStack {
			parts = append(parts, attr.Name)
		} else {
			modules = append(modules, attr.Name)
		}
	}

	return modules, parts, diags
}

// decodePart decodes a stack block of a stack: source, version, values,
// depends_on.
func decodePart(b *hcl.Block) (*StackPart, hcl.Diagnostics) {
	name := b.Labels[0]
	if !releaseName.MatchString(name) {
		return nil, hcl.Diagnostics{errorAt(b.LabelRanges[0], "Invalid part name",
			fmt.Sprintf("%q is not a name of a part: lower case letters, digits and -, at most 63.", name))}
	}

	attrs, diags := b.Body.JustAttributes()
	if diags.HasErrors() {
		return nil, diags
	}

	p := &StackPart{Name: name, Block: b}

	for n, attr := range attrs {
		var moreDiags hcl.Diagnostics

		switch n {
		case attrSource:
			moreDiags = gohcl.DecodeExpression(attr.Expr, nil, &p.Source)
		case attrVersion:
			moreDiags = gohcl.DecodeExpression(attr.Expr, nil, &p.Version)
		case attrValues:
			p.Values = attr.Expr
		case "depends_on":
			p.DependsOn, p.DependsOnParts, moreDiags = decodeDependsOn(attr)
		default:
			moreDiags = hcl.Diagnostics{errorAt(attr.NameRange, "Unknown attribute",
				"A stack block takes source, version, values and depends_on, not "+n+".")}
		}

		diags = append(diags, moreDiags...)
	}

	if p.Source == "" && !diags.HasErrors() {
		diags = append(diags, errorAt(b.DefRange, "Missing source", "A part names the source of a published stack."))
	}

	return p, diags
}

// parsePart parses the source of a part: a published stack of the global
// registry or of a git repository.
func (s *Stack) parsePart(p *StackPart) hcl.Diagnostics {
	registries, diags := s.Registries()
	if diags.HasErrors() {
		return diags
	}

	req, err := source.Parse(p.Source, registries)

	switch {
	case err != nil:
		return hcl.Diagnostics{errorAt(p.Block.DefRange, "Invalid source", err.Error()+".")}
	case req.Kind != source.KindGlobal && req.Kind != source.KindGit:
		return hcl.Diagnostics{errorAt(p.Block.DefRange, "Invalid source",
			p.Source+" is not a published stack of a registry: <namespace>/<name> or a git address (design 7.5).")}
	}

	req.Stack = true

	switch {
	case req.Kind == source.KindGlobal:
		req.Path = "stacks/" + req.Name
	case req.Registry != "":
		req.Path = "stacks/" + path.Base(req.Path)
	}

	p.Address = &req

	return nil
}

// Registries returns the repositories of the registries of the stack by
// name.
func (s *Stack) Registries() (map[string]string, hcl.Diagnostics) {
	out := map[string]string{}

	var diags hcl.Diagnostics

	for _, b := range s.Rigger.Registries {
		var reg struct {
			Source string `hcl:"source"`
		}

		if moreDiags := gohcl.DecodeBody(b.Body, nil, &reg); moreDiags.HasErrors() {
			diags = append(diags, moreDiags...)

			continue
		}

		out[b.Labels[0]] = reg.Source
	}

	return out, diags
}

// loadRelease loads the module of a release with a local source, and
// parses the address of any other.
func (s *Stack) loadRelease(ctx context.Context, root fs.FS, r *StackRelease, limits Limits) hcl.Diagnostics {
	rng := r.Block.DefRange

	if !strings.HasPrefix(r.Source, "./") && !strings.HasPrefix(r.Source, "../") {
		registries, diags := s.Registries()
		if diags.HasErrors() {
			return diags
		}

		req, err := source.Parse(r.Source, registries)
		if err != nil {
			return hcl.Diagnostics{errorAt(rng, "Invalid source", err.Error()+".")}
		}

		r.Address = &req

		return nil
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

	if err := CheckPath(root, s.Dir, r.Source); err != nil {
		return hcl.Diagnostics{errorAt(rng, "Invalid source", err.Error()+".")}
	}

	m, diags := loadTree(ctx, root, dir, r.Source, limits, nil)
	r.Module = m

	if m.Metadata.Library {
		diags = append(diags, errorAt(rng, "Library module",
			fmt.Sprintf("The module %q is a library; another module calls it, a stack does not install it.", m.Metadata.Name)))
	}

	return diags
}

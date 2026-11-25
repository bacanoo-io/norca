// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"io/fs"
	"log/slog"
	"slices"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/bacanoo-io/norca/rigger/internal/eval"
	"github.com/bacanoo-io/norca/rigger/internal/lang"
	"github.com/bacanoo-io/norca/rigger/module"
	"github.com/bacanoo-io/norca/rigger/oplog"
	"github.com/bacanoo-io/norca/rigger/resource"
)

// The icon of a module (design 7.4).
const (
	iconFile  = "icon.png"
	iconSize  = 64
	iconBytes = 32 << 10
)

// Module is a loaded module. Nothing changes it after [Engine.LoadModule],
// so it is safe for concurrent use.
type Module struct {
	mod  *lang.Module
	fsys fs.FS

	// origin and fetched are set for a module fetched from a source
	// (FetchModule): what a release records of it, and what verified it.
	origin  *moduleOrigin
	fetched *ModuleOrigin
}

// Digest returns the tree digest of the module and its local child modules
// (design 8.1): what a signature signs.
func (m *Module) Digest() (string, error) {
	return lang.Digest(m.mod)
}

// Dir is the directory of the module in the root it was loaded from.
func (m *Module) Dir() string {
	return m.mod.Dir
}

// Files returns what a module is published from: the root it was loaded
// from, and the directories of the module and of its child modules there.
func (m *Module) Files() (fs.FS, []string) {
	return m.mod.Root, moduleDirs(m.mod)
}

// Metadata returns the module block of the module.
func (m *Module) Metadata() module.Metadata {
	return m.mod.Metadata
}

// Tabs returns the tabs of the install form. The slice is a copy.
func (m *Module) Tabs() []module.Tab {
	return slices.Clone(m.mod.Tabs)
}

// Variables returns the variables of the module in the order of
// variables.hcl.
func (m *Module) Variables() []module.Variable {
	return variablesOf(m.mod)
}

// Outputs returns the outputs of the module in the order of outputs.hcl,
// with what they say of themselves; nothing is evaluated.
func (m *Module) Outputs() []module.Output {
	return outputsOf(m.mod.Outputs)
}

// Readme returns the README.md of the module directory; the error matches
// fs.ErrNotExist when it has none.
func (m *Module) Readme() ([]byte, error) {
	return fs.ReadFile(m.mod.FS, "README.md") // FS is the module directory
}

// variablesOf describes the variables of mod in the order of variables.hcl.
func variablesOf(mod *lang.Module) []module.Variable {
	if mod == nil {
		return nil
	}

	out := make([]module.Variable, 0, len(mod.Variables))
	for _, v := range mod.Variables {
		out = append(out, module.Variable{
			Name:        v.Name,
			Description: v.Description,
			Type:        v.TypeText,
			Default:     defaultText(v),
			Required:    v.Default == cty.NilVal,
			Sensitive:   v.Sensitive,
		})
	}

	return out
}

// defaultText is the default of a variable as JSON text; empty for none,
// for a sensitive variable, and for one that is not known before an
// evaluation.
func defaultText(v *lang.Variable) string {
	if v.Default == cty.NilVal || v.Sensitive || !v.Default.IsWhollyKnown() {
		return ""
	}

	// A null of no type would be written as JSON of a dynamic value.
	if v.Default.IsNull() {
		return "null"
	}

	data, err := ctyjson.Marshal(v.Default, v.Default.Type())
	if err != nil {
		return ""
	}

	return string(data)
}

// outputsOf describes the output blocks of outputs.hcl, in their order: the
// label, and the description and sensitive attributes as they are written.
// An attribute that is not a literal of its type says nothing; the
// evaluation reports it.
func outputsOf(blocks []*hcl.Block) []module.Output {
	schema := &hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "description"}, {Name: "sensitive"}}}

	out := make([]module.Output, 0, len(blocks))

	for _, b := range blocks {
		if len(b.Labels) == 0 {
			continue
		}

		o := module.Output{Name: b.Labels[0]}

		content, _, _ := b.Body.PartialContent(schema)
		if content != nil {
			if attr, ok := content.Attributes["description"]; ok {
				_ = gohcl.DecodeExpression(attr.Expr, nil, &o.Description) // stays empty on an error
			}

			if attr, ok := content.Attributes["sensitive"]; ok {
				_ = gohcl.DecodeExpression(attr.Expr, nil, &o.Sensitive) // stays false on an error
			}
		}

		out = append(out, o)
	}

	return out
}

// Value is a value given for a variable. Range says where it was written; it
// is nil for a value given without a place, such as a command line flag.
type Value struct {
	Value cty.Value
	Range *hcl.Range
}

// Values are the values given for the variables of a module, by name.
type Values map[string]Value

// ParseValuesJSON parses values given as one JSON object, a member per
// variable, as the form of a UI sends them.
func ParseValuesJSON(filename string, src []byte) (_ Values, retDiags hcl.Diagnostics) {
	defer func() {
		// No engine, so no log: the diagnostic is all there is.
		if r := recover(); r != nil {
			retDiags = errorDiags("Internal error", ErrInternal)
		}
	}()

	inputs, diags := eval.ParseValuesJSON(filename, src)
	values := make(Values, len(inputs))

	for name, in := range inputs {
		values[name] = Value{Value: in.Value, Range: in.Range}
	}

	return values, diags
}

// ParseValues parses a file of values (a *.vars.hcl): one attribute per
// variable, each a constant expression.
func ParseValues(filename string, src []byte) (_ Values, retDiags hcl.Diagnostics) {
	defer func() {
		// No engine, so no log: the diagnostic is all there is.
		if r := recover(); r != nil {
			retDiags = errorDiags("Internal error", ErrInternal)
		}
	}()

	inputs, diags := eval.ParseValues(filename, src)
	values := make(Values, len(inputs))

	for name, in := range inputs {
		values[name] = Value{Value: in.Value, Range: in.Range}
	}

	return values, diags
}

// LoadModule loads the module whose files fsys holds, with the module's
// directory as its root (design 4.5). It reads nothing outside fsys and
// stops when ctx is cancelled; the limits of the engine bound what it reads.
func (e *Engine) LoadModule(ctx context.Context, fsys fs.FS) (_ *Module, retDiags hcl.Diagnostics) {
	defer e.recoverDiags(ctx, "module.load", &retDiags)

	return e.LoadModuleAt(ctx, fsys, ".")
}

// LoadModuleAt loads the module in the directory dir of root, and its child
// modules (design 6.5). A child module comes from a local source, a
// directory relative to the module that calls it (../lib/placement), and is
// read from root too: nothing outside root is read.
func (e *Engine) LoadModuleAt(ctx context.Context, root fs.FS, dir string) (_ *Module, retDiags hcl.Diagnostics) {
	defer e.recoverDiags(ctx, "module.load", &retDiags)

	ctx, span := oplog.Start(ctx, e.logger, slog.LevelDebug, "module.load", "dir", dir)

	mod, diags := lang.LoadAt(ctx, root, dir, e.langLimits())
	span.End(ctx, diagsErr(diags))

	return &Module{mod: mod, fsys: mod.FS}, diags
}

// CheckValues checks values against the variables of m: undeclared names,
// missing values, types, validations and rules (design 6.1, 6.2).
func (e *Engine) CheckValues(ctx context.Context, m *Module, values Values) (retDiags hcl.Diagnostics) {
	defer e.recoverDiags(ctx, "module.values", &retDiags)

	_, diags := e.variables(ctx, m, values)

	return diags
}

// variables gives the variables of m their values and checks them; it
// returns the object var.* of an evaluation.
func (e *Engine) variables(ctx context.Context, m *Module, values Values) (cty.Value, hcl.Diagnostics) {
	ctx, span := oplog.Start(ctx, e.logger, slog.LevelDebug, "module.values", "module", m.mod.Metadata.Name)

	inputs := make(map[string]eval.Input, len(values))
	for name, v := range values {
		inputs[name] = eval.Input{Value: v.Value, Range: v.Range}
	}

	vars, diags := eval.Variables(m.mod, inputs)
	span.End(ctx, diagsErr(diags))

	return vars, diags
}

// ValidateModule checks what can be checked of a module without a cluster
// (design 10, rigger module validate): its icon, its test files (parsed, not
// run), every value file of its examples/, checked against its variables
// and evaluated (design 7.4), the module with no values when every variable
// has a default, and a warning for each variable that changes
// nothing (design 14.1). A module with a job is evaluated only by an engine
// with a job decoder.
func (e *Engine) ValidateModule(ctx context.Context, m *Module) (retDiags hcl.Diagnostics) {
	defer e.recoverDiags(ctx, "module.validate", &retDiags)

	ctx, span := oplog.Start(ctx, e.logger, slog.LevelInfo, "module.validate",
		"module", m.mod.Metadata.Name, "version", m.mod.Metadata.Version)

	diags := e.checkIcon(m)
	diags = append(diags, lang.UnusedVariables(m.mod)...)

	for _, p := range m.mod.Tests {
		_, moreDiags := e.parseTests(m, p)
		diags = append(diags, moreDiags...)
	}

	for _, p := range m.mod.Examples {
		if err := ctx.Err(); err != nil {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: "Validation cancelled", Detail: err.Error() + "."})

			break
		}

		diags = append(diags, e.checkExample(ctx, m, p)...)
	}

	diags = append(diags, e.checkDefaults(ctx, m)...)

	span.End(ctx, diagsErr(diags))

	return diags
}

// checkExample checks the value file at p.
func (e *Engine) checkExample(ctx context.Context, m *Module, p string) hcl.Diagnostics {
	src, err := fs.ReadFile(m.fsys, p)
	if err != nil {
		return hcl.Diagnostics{{Severity: hcl.DiagError, Summary: "Example not read", Detail: err.Error() + "."}}
	}

	values, diags := ParseValues(p, src)
	if diags.HasErrors() {
		return diags
	}

	if !e.canEvaluate(m) {
		return append(diags, e.CheckValues(ctx, m, values)...)
	}

	md := m.mod.Metadata
	_, moreDiags := e.evaluate(ctx, m, Install{Release: md.Name, Namespace: "default", Region: "global", Values: values})

	return append(diags, moreDiags...)
}

// checkDefaults evaluates a module with no values when every variable has
// a default: the install that gives none must work too, examples or not.
func (e *Engine) checkDefaults(ctx context.Context, m *Module) hcl.Diagnostics {
	if !e.canEvaluate(m) {
		return nil
	}

	for _, v := range m.mod.Variables {
		if v.Default == cty.NilVal {
			return nil
		}
	}

	md := m.mod.Metadata
	_, diags := e.evaluate(ctx, m, Install{Release: md.Name, Namespace: "default", Region: "global"})

	return diags
}

// canEvaluate reports whether the engine can evaluate m: m is not a
// library, and the engine has a job decoder or m has no job.
func (e *Engine) canEvaluate(m *Module) bool {
	if m.mod.Metadata.Library {
		return false
	}

	return e.jobs != nil || !hasJob(m.mod)
}

// hasJob reports whether m or one of its children has a job.
func hasJob(m *lang.Module) bool {
	for _, r := range m.Resources {
		if r.Type.Kind == resource.KindJob {
			return true
		}
	}

	for _, c := range m.Children {
		if hasJob(c.Module) {
			return true
		}
	}

	return false
}

// checkIcon checks the icon of a module, if it has one: a PNG of 64 by 64
// pixels and at most 32 KiB (design 7.4).
func (e *Engine) checkIcon(m *Module) hcl.Diagnostics {
	name := m.mod.Metadata.Icon
	if name == "" {
		name = iconFile
	}

	if err := lang.CheckPath(m.fsys, ".", name); err != nil {
		return iconDiag(fmt.Sprintf("The icon %s is not taken: %s.", name, err))
	}

	data, err := fs.ReadFile(m.fsys, name)
	if err != nil {
		if m.mod.Metadata.Icon == "" {
			return nil
		}

		return iconDiag(fmt.Sprintf("The icon %s is not read: %s.", name, err))
	}

	if len(data) > iconBytes {
		return iconDiag(fmt.Sprintf("The icon %s has %d bytes, more than %d.", name, len(data), iconBytes))
	}

	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return iconDiag(fmt.Sprintf("The icon %s is not a PNG: %s.", name, err))
	}

	if cfg.Width != iconSize || cfg.Height != iconSize {
		return iconDiag(fmt.Sprintf("The icon %s is %dx%d pixels, not %dx%d.", name, cfg.Width, cfg.Height, iconSize, iconSize))
	}

	return nil
}

func iconDiag(detail string) hcl.Diagnostics {
	return hcl.Diagnostics{{Severity: hcl.DiagError, Summary: "Invalid icon", Detail: detail}}
}

// diagsErr returns the diagnostics as an error when they hold one, for the
// end line of a span.
func diagsErr(diags hcl.Diagnostics) error {
	if diags.HasErrors() {
		return diags
	}

	return nil
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package convert does the mechanical part of turning a pack of Norca's
// pack manager into a Rigger module (design 14): the metadata, the variables
// with their checks, the examples, and the templates kept as comments for a
// person to rewrite as HCL.
package convert

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// The variables of a pack that are the release's in a module (design 6.4).
var releaseVariables = []string{"namespace", "region"}

// Result is what a conversion wrote, and what a person has to look at.
type Result struct {
	// Files are the files written, relative to the output directory.
	Files []string
	// Notes are the places the conversion could not finish.
	Notes []string
}

// Pack converts the pack in dir into a module in out, which must not exist
// or be empty. examples, when not empty, is a directory of variable files
// of the pack; each becomes examples/<name>.vars.hcl of the module.
func Pack(dir, examples, out string) (*Result, error) {
	if err := emptyDir(out); err != nil {
		return nil, err
	}

	if examples != "" {
		if _, err := os.ReadDir(examples); err != nil {
			return nil, err
		}
	}

	c := &converter{dir: dir, out: out, res: &Result{}}

	steps := []func() error{c.metadata, c.variables, c.templates, c.readme}
	if examples != "" {
		steps = append(steps, func() error { return c.examples(examples) })
	}

	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}

	sort.Strings(c.res.Files)

	return c.res, nil
}

func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return err
	}

	if len(entries) > 0 {
		return fmt.Errorf("%s is not empty", dir)
	}

	return nil
}

type converter struct {
	dir, out string
	res      *Result
	header   []byte
}

func (c *converter) note(format string, args ...any) {
	c.res.Notes = append(c.res.Notes, fmt.Sprintf(format, args...))
}

// write writes an HCL file of the module, formatted.
func (c *converter) write(name string, data []byte) error {
	return c.writeRaw(name, tidy(hclwrite.Format(data)))
}

// writeRaw writes a file of the module as it is.
func (c *converter) writeRaw(name string, data []byte) error {
	path := filepath.Join(c.out, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}

	if err := os.WriteFile(path, data, 0o600); err != nil { //nolint:gosec // the directory the caller named
		return err
	}

	c.res.Files = append(c.res.Files, name)

	return nil
}

// tidy leaves at most one empty line between two lines of HCL: a removed
// block leaves its own. The empty lines of a heredoc and of a comment are
// text of the pack, and stay; data that is not HCL stays as it is.
func tidy(data []byte) []byte {
	tokens, diags := hclsyntax.LexConfig(data, "", hcl.InitialPos)
	if diags.HasErrors() {
		return data
	}

	out := make([]byte, 0, len(data))
	end, heredocs := 0, 0

	for _, tok := range tokens {
		// Between two tokens there are spaces, which are no tokens.
		out = append(out, data[end:tok.Range.Start.Byte]...)
		end = tok.Range.End.Byte

		// The text of a heredoc is tokens of its own; a new line in it is
		// one of an expression of its template.
		if tok.Type == hclsyntax.TokenOHeredoc {
			heredocs++
		}

		if tok.Type == hclsyntax.TokenCHeredoc {
			heredocs--
		}

		if tok.Type == hclsyntax.TokenNewline && heredocs == 0 && bytes.HasSuffix(out, []byte("\n\n")) {
			continue
		}

		out = append(out, tok.Bytes...)
	}

	return append(out, data[end:]...)
}

// parse reads an HCL file of the pack for writing.
func parse(path string) (*hclwrite.File, error) {
	src, err := os.ReadFile(path) //nolint:gosec // a file of the pack being converted
	if err != nil {
		return nil, err
	}

	f, diags := hclwrite.ParseConfig(src, path, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, diags
	}

	return f, nil
}

// leadingComment is the block of comment lines a file starts with (its
// licence notice, as a rule).
func leadingComment(src []byte) []byte {
	var out bytes.Buffer

	for line := range strings.Lines(string(src)) {
		if !strings.HasPrefix(line, "#") {
			break
		}

		out.WriteString(line)
	}

	return out.Bytes()
}

// metadata writes metadata.hcl: the app and pack blocks become the module
// block; requires and the tabs of the form stay as they are.
func (c *converter) metadata() error {
	path := filepath.Join(c.dir, "metadata.hcl")

	src, err := os.ReadFile(path) //nolint:gosec // a file of the pack being converted
	if err != nil {
		return err
	}

	c.header = leadingComment(src)

	f, err := parse(path)
	if err != nil {
		return err
	}

	out := hclwrite.NewEmptyFile()
	module := out.Body().AppendNewBlock("module", nil).Body()

	var (
		pack, app *hclwrite.Block
		rest      []*hclwrite.Block
	)

	for _, b := range f.Body().Blocks() {
		switch b.Type() {
		case "pack":
			pack = b
		case "app":
			app = b
		default:
			rest = append(rest, b)
		}
	}

	if pack == nil {
		return fmt.Errorf("%s has no pack block", path)
	}

	for _, name := range []string{"name", "version", "description"} {
		if a := pack.Body().GetAttribute(name); a != nil {
			module.SetAttributeRaw(name, a.Expr().BuildTokens(nil))
		}
	}

	if app != nil {
		if a := app.Body().GetAttribute("url"); a != nil {
			module.SetAttributeRaw("url", a.Expr().BuildTokens(nil))
		}
	}

	for _, b := range pack.Body().Blocks() {
		module.AppendNewline()
		module.AppendBlock(b)
	}

	for _, b := range rest {
		out.Body().AppendNewline()
		out.Body().AppendBlock(b)
	}

	return c.write("metadata.hcl", append(append([]byte{}, c.header...), append([]byte("\n"), out.Bytes()...)...))
}

// variables writes variables.hcl: the variables of the release go, and the
// checks are written as Rigger takes them.
func (c *converter) variables() error {
	path := filepath.Join(c.dir, "variables.hcl")

	f, err := parse(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return err
	}

	for _, b := range f.Body().Blocks() {
		switch b.Type() {
		case "variable":
			if len(b.Labels()) == 1 && isReleaseVariable(b.Labels()[0]) {
				f.Body().RemoveBlock(b)

				continue
			}

			if err := c.variable(b); err != nil {
				return err
			}
		case "rule":
		default:
			c.note("variables.hcl: a %s block is left as it is", b.Type())
		}
	}

	data := f.Bytes()
	for _, name := range releaseVariables {
		if bytes.Contains(data, []byte("var."+name)) {
			c.note("variables.hcl: var.%s is the release's now: rigger.%s", name, name)
		}
	}

	return c.write("variables.hcl", data)
}

func isReleaseVariable(name string) bool { return slices.Contains(releaseVariables, name) }

// The checks of the pack manager on the items of a list and what they are
// in an item block of Rigger (design 6.2).
var itemChecks = map[string]string{"item_min_length": "min_length", "item_pattern": "pattern"}

// variable rewrites the validation and ui blocks of one variable.
func (c *converter) variable(v *hclwrite.Block) error {
	vb := &varBlocks{v: v}

	for _, b := range v.Body().Blocks() {
		switch b.Type() {
		case "validation":
			vb.validation = b
		case "ui":
			vb.ui = b
		}
	}

	if vb.validation != nil {
		for _, old := range []string{"item_min_length", "item_pattern"} {
			if a := vb.validation.Body().GetAttribute(old); a != nil {
				tokens := a.Expr().BuildTokens(nil)

				vb.validation.Body().RemoveAttribute(old)
				vb.item().SetAttributeRaw(itemChecks[old], tokens)
			}
		}
	}

	if vb.ui == nil {
		return nil
	}

	if err := vb.options(); err != nil {
		return fmt.Errorf("variable %q: %w", v.Labels()[0], err)
	}

	if len(vb.ui.Body().Attributes()) == 0 && len(vb.ui.Body().Blocks()) == 0 {
		v.Body().RemoveBlock(vb.ui)
	}

	return nil
}

// varBlocks are the blocks of a variable being rewritten; validation and
// its item block are made when a check needs them.
type varBlocks struct {
	v, validation, ui *hclwrite.Block
	itemBody          *hclwrite.Body
}

func (vb *varBlocks) checks() *hclwrite.Body {
	if vb.validation == nil {
		vb.validation = vb.v.Body().AppendNewBlock("validation", nil)
		if vb.ui != nil {
			// Before the hints of the form, as a person would write it.
			vb.v.Body().RemoveBlock(vb.ui)
			vb.v.Body().AppendNewline()
			vb.v.Body().AppendBlock(vb.ui)
		}
	}

	return vb.validation.Body()
}

func (vb *varBlocks) item() *hclwrite.Body {
	if vb.itemBody != nil {
		return vb.itemBody
	}

	body := vb.checks()
	for _, b := range body.Blocks() {
		if b.Type() == "item" {
			vb.itemBody = b.Body()

			return vb.itemBody
		}
	}

	vb.itemBody = body.AppendNewBlock("item", nil).Body()

	return vb.itemBody
}

// options moves the options of a choice into the checks (design 14.1): a
// value outside them is refused, and the form takes them from there.
func (vb *varBlocks) options() error {
	ui := vb.ui.Body()

	if a := ui.GetAttribute("options"); a != nil {
		vb.checks().SetAttributeRaw("one_of", a.Expr().BuildTokens(nil))
		ui.RemoveAttribute("options")
	}

	a := ui.GetAttribute("item_options")
	if a == nil {
		return nil
	}

	val, err := constant(a)
	if err != nil {
		return fmt.Errorf("item_options: %w", err)
	}

	if !val.Type().IsObjectType() && !val.Type().IsMapType() {
		return errors.New("item_options is not an object")
	}

	for it := val.ElementIterator(); it.Next(); {
		k, options := it.Element()
		vb.item().AppendNewBlock("attribute", []string{k.AsString()}).Body().SetAttributeValue("one_of", options)
	}

	ui.RemoveAttribute("item_options")

	return nil
}

// constant is the value of an attribute that names nothing.
func constant(a *hclwrite.Attribute) (cty.Value, error) {
	src := a.Expr().BuildTokens(nil).Bytes()

	expr, diags := hclsyntax.ParseExpression(src, "", hcl.InitialPos)
	if diags.HasErrors() {
		return cty.NilVal, diags
	}

	val, diags := expr.Value(nil)
	if diags.HasErrors() {
		return cty.NilVal, diags
	}

	return val, nil
}

// template is a template of the pack and the file of the module it is kept
// in as a comment.
type template struct{ src, dst string }

// templates keeps every template of the pack as the comment of the file it
// becomes: templates/<name>.nomad.tpl the job file <name>.norca.hcl, the
// helpers locals.hcl, outputs.tpl outputs.hcl.
func (c *converter) templates() error {
	list, err := c.templateList()
	if err != nil {
		return err
	}

	written := map[string]*bytes.Buffer{}

	var order []string

	for _, t := range list {
		src, err := os.ReadFile(filepath.Join(c.dir, t.src))
		if err != nil {
			return err
		}

		buf := written[t.dst]
		if buf == nil {
			buf = bytes.NewBuffer(append([]byte{}, c.header...))
			written[t.dst] = buf
			order = append(order, t.dst)
		}

		commentOut(buf, filepath.ToSlash(t.src), src)
		c.note("%s: the template of %s is to be written as HCL", t.dst, filepath.ToSlash(t.src))
	}

	for _, name := range order {
		// Comments only: written as they are, not formatted.
		if err := c.writeRaw(name, written[name].Bytes()); err != nil {
			return err
		}
	}

	return nil
}

func (c *converter) templateList() ([]template, error) {
	var list []template

	entries, err := os.ReadDir(filepath.Join(c.dir, "templates"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".tpl") {
			continue
		}

		dst := strings.TrimSuffix(name, ".tpl")

		switch {
		case strings.HasPrefix(name, "_"):
			dst = "locals.hcl"
		case strings.HasSuffix(dst, ".hcl"):
			// A policy, a kind, an admission policy: the file names of a
			// module are those of the pack's (dns.acl.hcl).
		default:
			dst = strings.TrimSuffix(dst, ".nomad") + ".norca.hcl"
		}

		list = append(list, template{src: filepath.Join("templates", name), dst: dst})
	}

	if _, err := os.Stat(filepath.Join(c.dir, "outputs.tpl")); err == nil {
		list = append(list, template{src: "outputs.tpl", dst: "outputs.hcl"})
	}

	return list, nil
}

// commentOut writes src, from the file name of the pack, as comment lines.
func commentOut(buf *bytes.Buffer, name string, src []byte) {
	fmt.Fprintf(buf, "\n# Converted by rigger convert pack from %s of the pack: the template\n# below is to be written as HCL.\n#\n", name)

	for line := range strings.Lines(string(src)) {
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			buf.WriteString("#\n")

			continue
		}

		buf.WriteString("# " + line + "\n")
	}
}

func (c *converter) readme() error {
	src, err := os.ReadFile(filepath.Join(c.dir, "README.md"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return err
	}

	if err := c.writeRaw("README.md", src); err != nil {
		return err
	}

	c.note("README.md: the commands and names of the pack are to be written for the module")

	return nil
}

// examples writes each variable file of dir as examples/<name>.vars.hcl,
// without the variables of the release.
func (c *converter) examples(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".hcl") {
			continue
		}

		f, err := parse(filepath.Join(dir, name))
		if err != nil {
			return err
		}

		for _, v := range releaseVariables {
			if f.Body().GetAttribute(v) != nil {
				f.Body().RemoveAttribute(v)
				c.note("examples/%s: %s is the release's now", name, v)
			}
		}

		base := strings.TrimSuffix(strings.TrimSuffix(name, ".hcl"), ".vars")
		if err := c.write("examples/"+base+".vars.hcl", f.Bytes()); err != nil {
			return err
		}
	}

	return nil
}

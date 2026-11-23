// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/cli/norcaapi"
)

// errOneDirectory is the error of a render given more than one directory.
var errOneDirectory = errors.New("module render takes one directory")

// renderFlags are the flags of rigger module render.
type renderFlags struct {
	dir       string
	files     listFlag
	vars      listFlag
	release   string
	namespace string
	region    string
}

// listFlag is a flag that may be given more than once.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	*l = append(*l, v)

	return nil
}

// parseRenderFlags parses [dir] [flags], or [flags] [dir].
func parseRenderFlags(stderr io.Writer, args []string) (*renderFlags, error) {
	f := &renderFlags{dir: "."}

	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		f.dir, args = args[0], args[1:]
	}

	flags := flag.NewFlagSet("rigger module render", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Var(&f.files, "f", "a file of values (*.vars.hcl); may be given more than once, a later one wins")
	flags.Var(&f.vars, "var", "a value, name=value; wins over the files; may be given more than once"+varSyntax)
	flags.StringVar(&f.release, "release", "", "name of the release (default: the name of the module)")
	flags.StringVar(&f.namespace, "namespace", "default", "namespace of the release")
	flags.StringVar(&f.region, "region", "global", "region of the release")

	if err := flags.Parse(args); err != nil {
		return nil, err
	}

	switch flags.NArg() {
	case 0:
	case 1:
		if f.dir != "." {
			return nil, errOneDirectory
		}

		f.dir = flags.Arg(0)
	default:
		return nil, errOneDirectory
	}

	return f, nil
}

// renderModule evaluates the module of f.dir with the values of f, and
// writes its resources and outputs to stdout as JSON. The jobs are decoded
// with the job decoder of Norca.
func renderModule(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, f *renderFlags) error {
	eng, err := rigger.New(rigger.WithLogger(logger), rigger.WithJobDecoder(norcaapi.JobDecoder{}), rigger.WithResourceDecoder(norcaapi.ResourceDecoder{}))
	if err != nil {
		return err
	}

	m, diags, err := loadModule(ctx, eng, f.dir)
	if err != nil {
		return err
	}

	if diags.HasErrors() {
		return reportDiags(stderr, f.dir, diags)
	}

	values, moreDiags := readValues(f, m.FormSchema())
	diags = append(diags, moreDiags...)

	if moreDiags.HasErrors() {
		return reportDiags(stderr, f.dir, diags)
	}

	release := f.release
	if release == "" {
		release = m.Metadata().Name
	}

	res, moreDiags := eng.Evaluate(ctx, m, rigger.Install{
		Release: release, Namespace: f.namespace, Region: f.region, Values: values,
	})
	diags = append(diags, moreDiags...)

	if diags.HasErrors() {
		return reportDiags(stderr, f.dir, diags)
	}

	if len(diags) > 0 {
		if err := writeDiags(stderr, f.dir, diags); err != nil {
			return err
		}
	}

	return writeRendered(stdout, m, release, f, res)
}

// rendered is the output of rigger module render.
type rendered struct {
	Module    string             `json:"module"`
	Version   string             `json:"version,omitempty"`
	Release   string             `json:"release"`
	Namespace string             `json:"namespace"`
	Region    string             `json:"region"`
	Resources []renderedResource `json:"resources"`
	Outputs   []rigger.Output    `json:"outputs"`
}

type renderedResource struct {
	rigger.Resource

	// SecretItems are the names of the secret items; their values are
	// never written.
	SecretItems []string `json:"secret_items,omitempty"`
}

func writeRendered(w io.Writer, m *rigger.Module, release string, f *renderFlags, res *rigger.Result) error {
	md := m.Metadata()
	out := rendered{
		Module: md.Name, Version: md.Version, Release: release, Namespace: f.namespace, Region: f.region,
		Resources: make([]renderedResource, 0, len(res.Resources)), Outputs: res.Outputs,
	}

	for _, r := range res.Resources {
		out.Resources = append(out.Resources, renderedResource{Resource: r, SecretItems: r.SecretItems()})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)

	return enc.Encode(out)
}

// readValues reads the files of values in order, then the -var values; a
// later value of a name wins. form is the install form of the module or
// stack the values are for (FormSchema), which has the type of each
// variable; nil when the variables are not known here (a published stack
// that the servers fetch).
func readValues(f *renderFlags, form map[string]any) (rigger.Values, hcl.Diagnostics) {
	values := rigger.Values{}

	var diags hcl.Diagnostics

	for _, p := range f.files {
		// The person running rigger names the file; it may be anywhere.
		src, err := os.ReadFile(filepath.Clean(p))
		if err != nil {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Values not read", Detail: err.Error() + ".",
			})

			continue
		}

		more, moreDiags := rigger.ParseValues(p, src)
		diags = append(diags, moreDiags...)

		maps.Copy(values, more)
	}

	types := variableTypes(form)

	for _, kv := range f.vars {
		name, v, ok := strings.Cut(kv, "=")
		if !ok || !hclsyntax.ValidIdentifier(name) {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Invalid -var",
				Detail: fmt.Sprintf("%q is not name=value.", kv),
			})

			continue
		}

		values[name] = rigger.Value{Value: varValue(v, types[name])}
	}

	return values, diags
}

// variableTypes returns the type of each variable of an install form (a
// JSON Schema), by name: string, number, integer, boolean, array or object,
// and none for a variable of any type.
func variableTypes(form map[string]any) map[string]string {
	props, _ := form["properties"].(map[string]any)
	types := make(map[string]string, len(props))

	for name, p := range props {
		if prop, ok := p.(map[string]any); ok {
			types[name], _ = prop["type"].(string)
		}
	}

	return types
}

// varSyntax ends the help of a -var flag: how its value is written.
const varSyntax = `. The value of a string, number or bool variable is the text as it is; of a list or object, HCL: 'tags=["a", "b"]'`

// primitiveTypes are the types of an install form whose value is one text.
var primitiveTypes = []string{"string", "number", "integer", "boolean"}

// varValue is the value of a -var for a variable of a type of the install
// form (variableTypes).
//
// For a string, a number or a bool the text is the value as it is written:
// the engine converts it to the type of the variable, so 1.20 stays "1.20"
// for a string and is 1.2 for a number. Reading it as HCL first would
// rewrite it (1.20 to 1.2, 007 to 7, 1e3 to 1000) before the type is known.
//
// For a list or an object the text is a constant expression of HCL
// (["a", "b"], { a = 1 }).
//
// Without a type (a variable of any type, or one that is not known here)
// the text is guessed: a constant expression of HCL (3, true, ["a", "b"]),
// or else the text as a string (redis, eu-1). A number is taken only when
// it is written as HCL writes it; any other spelling stays the text, which
// still converts to the number where one is needed.
func varValue(text, jsonType string) cty.Value {
	if slices.Contains(primitiveTypes, jsonType) {
		return cty.StringVal(text)
	}

	expr, diags := hclsyntax.ParseExpression([]byte(text), "-var", hcl.InitialPos)
	if diags.HasErrors() {
		return cty.StringVal(text)
	}

	v, diags := expr.Value(nil)
	if diags.HasErrors() {
		return cty.StringVal(text)
	}

	if jsonType == "" && !writtenAs(v, text) {
		return cty.StringVal(text)
	}

	return v
}

// writtenAs reports whether text is how a number is written when it is
// converted to a string; true for a value that is not a number.
func writtenAs(v cty.Value, text string) bool {
	if v.IsNull() || v.Type() != cty.Number {
		return true
	}

	s, err := convert.Convert(v, cty.String)

	return err == nil && s.AsString() == text
}

// reportDiags writes diagnostics and returns errReported.
// writeWarnings writes the diagnostics of an operation that went on: its
// warnings (advisories of the global registry, unused variables).
func writeWarnings(w io.Writer, dir string, diags hcl.Diagnostics) error {
	if len(diags) == 0 {
		return nil
	}

	return writeDiags(w, dir, diags)
}

func reportDiags(w io.Writer, dir string, diags hcl.Diagnostics) error {
	if err := writeDiags(w, dir, diags); err != nil {
		return err
	}

	return errReported
}

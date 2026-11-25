// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/spf13/cobra"
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
	files     []string
	vars      []string
	release   string
	namespace string
	region    string
}

// newRenderCommand is rigger module render [dir]: the directory may be
// before or after the flags.
func (a *app) newRenderCommand() *cobra.Command {
	f := &renderFlags{}

	cmd := &cobra.Command{
		Use:   "render [dir]",
		Short: "Evaluate a module and print its resources and outputs as JSON",
		Long: `Evaluate the module in dir (default: the current one) with the values of
--values and --var, and print its resources and outputs as JSON, for review.
The jobs are decoded as Norca decodes them; the items of a secret are named,
their values not shown. No cluster is needed.`,
		Example: `  rigger module render
  rigger module render modules/web --values examples/small.vars.hcl --var replicas=2
  rigger module render --release web-dev --namespace dev --region eu`,
		Args: oneDirectory(errOneDirectory),
		RunE: func(cmd *cobra.Command, args []string) error {
			f.dir = "."
			if len(args) > 0 {
				f.dir = args[0]
			}

			return renderModule(cmd.Context(), a.logger, a.stdout, a.stderr, f)
		},
	}

	flags := cmd.Flags()
	flags.StringArrayVarP(&f.files, valuesFlag, "f", nil, "a `file` of values (*.vars.hcl); may be given more than once, a later one wins")
	flags.StringArrayVar(&f.vars, varFlag, nil, "a value, `name=value`; wins over the files; may be given more than once"+varSyntax)
	flags.StringVar(&f.release, "release", "", "name of the release (default: the name of the module)")
	flags.StringVar(&f.namespace, namespaceFlag, "default", "namespace of the release")
	flags.StringVar(&f.region, regionFlag, "global", "region of the release")

	return cmd
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

// readValues reads the files of values in order, then the values of the
// environment (RIGGER_VAR_<name>), then the -var values; a later value of a
// name wins (design 6.1). form is the install form of the module or stack
// the values are for (FormSchema), which has the type of each variable; nil
// when the variables are not known here (a published stack that the
// servers fetch). The diagnostics may be warnings only: of a variable of
// the environment that is not used.
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

	diags = append(diags, envValues(values, os.Environ(), form != nil, types)...)

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

// envPrefix starts the name of a variable of the environment that gives the
// variable <name> its value: RIGGER_VAR_<name> (design 6.1). The name is as
// the variable is written, in its case.
const envPrefix = "RIGGER_VAR_"

// envValues puts the values that the environment gives into values: of
// each RIGGER_VAR_<name> whose name is a variable of types, read as a -var
// is (varValue). The core reads no environment (design 12.4); the program
// does, here.
//
// The environment is shared by every module and stack that is run in it,
// so a name that is no variable here is not an error, as it is for a -var:
// it is not used, and a warning says so. Without the variables (known is
// false: a published stack that the servers fetch) no name can be told
// from another, and none is used.
func envValues(values rigger.Values, environ []string, known bool, types map[string]string) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// In the order of the names: the warnings are the same on every run.
	for _, kv := range slices.Sorted(slices.Values(environ)) {
		key, v, _ := strings.Cut(kv, "=")

		name, ok := strings.CutPrefix(key, envPrefix)
		if !ok {
			continue
		}

		if _, declared := types[name]; declared {
			values[name] = rigger.Value{Value: varValue(v, types[name])}

			continue
		}

		why := fmt.Sprintf("there is no variable %q here", name)
		if !known {
			why = "the variables of a published stack that the servers fetch are not known here; give the value with -var"
		}

		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagWarning, Summary: "Variable of the environment not used",
			Detail: fmt.Sprintf("%s is not used: %s.", key, why),
		})
	}

	return diags
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
const varSyntax = `. The value of a string, number or bool variable is the text as it is; of a list or object, HCL: 'tags=["a", "b"]'. ` +
	`A variable of the environment RIGGER_VAR_<name> gives a value the same way; a --var wins over it`

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

// writeWarnings writes the diagnostics of an operation that went on: its
// warnings (advisories of the global registry, unused variables).
func writeWarnings(w io.Writer, dir string, diags hcl.Diagnostics) error {
	if len(diags) == 0 {
		return nil
	}

	return writeDiags(w, dir, diags)
}

// reportDiags writes diagnostics and returns errReported.
func reportDiags(w io.Writer, dir string, diags hcl.Diagnostics) error {
	if err := writeDiags(w, dir, diags); err != nil {
		return err
	}

	return errReported
}

// checkDiags is how a command goes on after a step of the engine: the
// errors of the step are written and end the command (errReported); without
// errors its warnings are written, and the command goes on. A step whose
// diagnostics are looked at for errors only loses its warnings.
func checkDiags(w io.Writer, dir string, diags hcl.Diagnostics) error {
	if diags.HasErrors() {
		return reportDiags(w, dir, diags)
	}

	return writeWarnings(w, dir, diags)
}

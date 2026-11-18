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
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	rig "github.com/bacanoo-io/norca-rig"
	"github.com/bacanoo-io/norca-rig/cli/norcaapi"
)

// errOneDirectory is the error of a render given more than one directory.
var errOneDirectory = errors.New("module render takes one directory")

// renderFlags are the flags of rig module render.
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

	flags := flag.NewFlagSet("rig module render", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Var(&f.files, "f", "a file of values (*.vars.hcl); may be given more than once, a later one wins")
	flags.Var(&f.vars, "var", "a value, name=value; wins over the files; may be given more than once")
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
	eng, err := rig.New(rig.WithLogger(logger), rig.WithJobDecoder(norcaapi.JobDecoder{}), rig.WithResourceDecoder(norcaapi.ResourceDecoder{}))
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

	values, moreDiags := readValues(f)
	diags = append(diags, moreDiags...)

	if moreDiags.HasErrors() {
		return reportDiags(stderr, f.dir, diags)
	}

	release := f.release
	if release == "" {
		release = m.Metadata().Name
	}

	res, moreDiags := eng.Evaluate(ctx, m, rig.Install{
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

// rendered is the output of rig module render.
type rendered struct {
	Module    string             `json:"module"`
	Version   string             `json:"version,omitempty"`
	Release   string             `json:"release"`
	Namespace string             `json:"namespace"`
	Region    string             `json:"region"`
	Resources []renderedResource `json:"resources"`
	Outputs   []rig.Output       `json:"outputs"`
}

type renderedResource struct {
	rig.Resource

	// SecretItems are the names of the secret items; their values are
	// never written.
	SecretItems []string `json:"secret_items,omitempty"`
}

func writeRendered(w io.Writer, m *rig.Module, release string, f *renderFlags, res *rig.Result) error {
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
// later value of a name wins.
func readValues(f *renderFlags) (rig.Values, hcl.Diagnostics) {
	values := rig.Values{}

	var diags hcl.Diagnostics

	for _, p := range f.files {
		// The person running rig names the file; it may be anywhere.
		src, err := os.ReadFile(filepath.Clean(p))
		if err != nil {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Values not read", Detail: err.Error() + ".",
			})

			continue
		}

		more, moreDiags := rig.ParseValues(p, src)
		diags = append(diags, moreDiags...)

		maps.Copy(values, more)
	}

	for _, kv := range f.vars {
		name, v, ok := strings.Cut(kv, "=")
		if !ok || !hclsyntax.ValidIdentifier(name) {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError, Summary: "Invalid -var",
				Detail: fmt.Sprintf("%q is not name=value.", kv),
			})

			continue
		}

		values[name] = rig.Value{Value: varValue(v)}
	}

	return values, diags
}

// varValue is the value of a -var: a constant expression of HCL (3, true,
// ["a", "b"]), or else the text as a string (redis, eu-1).
func varValue(text string) cty.Value {
	expr, diags := hclsyntax.ParseExpression([]byte(text), "-var", hcl.InitialPos)
	if diags.HasErrors() {
		return cty.StringVal(text)
	}

	v, diags := expr.Value(nil)
	if diags.HasErrors() {
		return cty.StringVal(text)
	}

	return v
}

// reportDiags writes diagnostics and returns errReported.
func reportDiags(w io.Writer, dir string, diags hcl.Diagnostics) error {
	if err := writeDiags(w, dir, diags); err != nil {
		return err
	}

	return errReported
}

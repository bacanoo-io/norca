// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

// moduleFunctions returns the functions of a module: the standard set, the
// files of the module, and the references the cluster resolves at run time
// (design 4.2). A file is read from fsys, the module's own files, and from
// nowhere else (design 12.4).
func moduleFunctions(fsys fs.FS, fileBytes int64) map[string]function.Function {
	funcs := Functions()
	funcs["file"] = fileFunc(fsys, fileBytes)
	funcs["templatefile"] = templateFileFunc(fsys, fileBytes, funcs)
	funcs["image_ref"] = imageRef
	funcs["runtime"] = runtimeFunc
	funcs["runtime_env"] = runtimeEnvFunc
	funcs["secret_ref"] = refFunc(secretRef)
	funcs["variable_ref"] = refFunc(variableRef)

	return funcs
}

// readModuleFile reads the file at p of the module: a path inside it, no
// larger than fileBytes.
func readModuleFile(fsys fs.FS, fileBytes int64, p string) ([]byte, error) {
	if !fs.ValidPath(p) {
		return nil, fmt.Errorf("%q is not a path inside the module", p)
	}

	info, err := fs.Stat(fsys, p)
	if err != nil {
		return nil, err
	}

	if info.Size() > fileBytes {
		return nil, fmt.Errorf("%s has %d bytes, more than %d", p, info.Size(), fileBytes)
	}

	return fs.ReadFile(fsys, p)
}

// fileFunc is file(path): the content of a file of the module.
func fileFunc(fsys fs.FS, fileBytes int64) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "path", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			data, err := readModuleFile(fsys, fileBytes, args[0].AsString())
			if err != nil {
				return cty.NilVal, err
			}

			return cty.StringVal(string(data)), nil
		},
	})
}

// templateFileFunc is templatefile(path, vars): a file of the module in the
// template syntax of HCL, which sees only vars (design 4.3).
func templateFileFunc(fsys fs.FS, fileBytes int64, funcs map[string]function.Function) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "path", Type: cty.String},
			{Name: "vars", Type: cty.DynamicPseudoType, AllowMarked: true},
		},
		Type: function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			p := args[0].AsString()

			data, err := readModuleFile(fsys, fileBytes, p)
			if err != nil {
				return cty.NilVal, err
			}

			vars := args[1]
			if !vars.Type().IsObjectType() && !vars.Type().IsMapType() {
				return cty.NilVal, function.NewArgErrorf(1, "vars must be an object")
			}

			expr, diags := hclsyntax.ParseTemplate(data, p, hcl.InitialPos)
			if diags.HasErrors() {
				return cty.NilVal, diags
			}

			ctx := &hcl.EvalContext{Variables: vars.AsValueMap(), Functions: funcs}

			v, diags := expr.Value(ctx)
			if diags.HasErrors() {
				return cty.NilVal, diags
			}

			return v, nil
		},
	})
}

// imageRef is image_ref(image, digest): the image pinned by its digest, or
// the image as it is when the digest is empty.
var imageRef = function.New(&function.Spec{
	Params: []function.Parameter{{Name: "image", Type: cty.String}, {Name: "digest", Type: cty.String}},
	Type:   function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
		image, digest := args[0].AsString(), args[1].AsString()
		if digest == "" {
			return cty.StringVal(image), nil
		}

		if repo, _, found := strings.Cut(image, "@"); found {
			image = repo
		}

		return cty.StringVal(image + "@" + digest), nil
	},
})

// runtimeFunc is runtime(reference): a reference the cluster resolves on the
// node, such as node.datacenter, as the job specification writes it.
var runtimeFunc = function.New(&function.Spec{
	Params: []function.Parameter{{Name: "reference", Type: cty.String}},
	Type:   function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
		return cty.StringVal("${" + args[0].AsString() + "}"), nil
	},
})

// runtimeEnvFunc is runtime_env(name): an environment variable of the task,
// as the job specification interpolates it.
var runtimeEnvFunc = function.New(&function.Spec{
	Params: []function.Parameter{{Name: "name", Type: cty.String}},
	Type:   function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
		return cty.StringVal("${" + args[0].AsString() + "}"), nil
	},
})

// refOptions are the options of secret_ref and variable_ref.
type refOptions struct {
	// quote is "conf" (a double-quoted string with \ and " escaped), "sql"
	// (a single-quoted string with ' doubled) or "" for the value as it is.
	quote string
	trim  bool
}

// refFunc returns secret_ref or variable_ref: write(path, item, options)
// gives the text of a task template that reads the item at run time and
// fails the template when it is missing.
func refFunc(write func(path, item string, o refOptions) string) function.Function {
	return function.New(&function.Spec{
		Params:   []function.Parameter{{Name: "path", Type: cty.String}, {Name: "item", Type: cty.String}},
		VarParam: &function.Parameter{Name: "options", Type: cty.DynamicPseudoType},
		Type:     function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			o, err := parseRefOptions(args[2:])
			if err != nil {
				return cty.NilVal, err
			}

			return cty.StringVal(write(args[0].AsString(), args[1].AsString(), o)), nil
		},
	})
}

func parseRefOptions(args []cty.Value) (refOptions, error) {
	var o refOptions

	if len(args) == 0 {
		return o, nil
	}

	if len(args) > 1 || !args[0].Type().IsObjectType() {
		return o, function.NewArgErrorf(2, "the options are one object: { quote, trim }")
	}

	for name, v := range args[0].AsValueMap() {
		if !o.set(name, v) {
			return o, function.NewArgErrorf(2, "unknown option %s: the options are quote (\"conf\", \"sql\" or false) and trim (a bool)", name)
		}
	}

	return o, nil
}

// set sets the option name to v, and says whether it is one.
func (o *refOptions) set(name string, v cty.Value) bool {
	v, _ = v.Unmark()
	if v.IsNull() || !v.IsKnown() {
		return false
	}

	switch {
	case name == "trim" && v.Type() == cty.Bool:
		o.trim = v.True()
	case name == "quote" && v.Type() == cty.Bool && v.False():
		o.quote = ""
	case name == "quote" && v.Type() == cty.String && (v.AsString() == "conf" || v.AsString() == "sql"):
		o.quote = v.AsString()
	default:
		return false
	}

	return true
}

// pipeline returns the template pipeline that trims and escapes a value.
func (o refOptions) pipeline() (prefix, filters, suffix string) {
	if o.trim {
		filters += " | trimSpace"
	}

	switch o.quote {
	case "conf":
		return `"`, filters + ` | replaceAll "\\" "\\\\" | replaceAll "\"" "\\\""`, `"`
	case "sql":
		return `'`, filters + ` | replaceAll "'" "''"`, `'`
	default:
		return "", filters, ""
	}
}

// secretRef reads an item of a secret of the built-in secret store.
func secretRef(path, item string, o refOptions) string {
	prefix, filters, suffix := o.pipeline()

	return fmt.Sprintf(`%s{{ with index (nomadSecret %s) %s }}{{ .%s }}{{ else }}{{ sprig_fail %s }}{{ end }}%s`,
		prefix, strconv.Quote(path), strconv.Quote(item), filters,
		strconv.Quote(fmt.Sprintf("the secret %s has no item %s", path, item)), suffix)
}

// variableRef reads an item of a Norca Variable.
func variableRef(path, item string, o refOptions) string {
	prefix, filters, suffix := o.pipeline()

	return fmt.Sprintf(`%s{{ with nomadVar %s }}{{ with index . %s }}{{ .Value%s }}{{ else }}{{ sprig_fail %s }}{{ end }}{{ end }}%s`,
		prefix, strconv.Quote(path), strconv.Quote(item), filters,
		strconv.Quote(fmt.Sprintf("the Variable %s has no item %s", path, item)), suffix)
}

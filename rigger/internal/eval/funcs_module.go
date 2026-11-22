// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"fmt"
	"io/fs"
	"maps"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"

	"github.com/bacanoo-io/norca/rigger/runref"
)

// moduleFunctions returns the functions of a module: the standard set, the
// files of the module, and the references the cluster resolves at run time
// (design 4.2). A file is read from fsys, the module's own files, and from
// nowhere else (design 12.4).
func moduleFunctions(fsys fs.FS, fileBytes int64) map[string]function.Function {
	funcs := Functions()
	funcs["file"] = fileFunc(fsys, fileBytes)
	funcs["image_ref"] = imageRef
	funcs["runtime"] = runtimeFunc
	funcs["runtime_env"] = runtimeEnvFunc
	funcs["secret_ref"] = itemFunc(runref.KindSecret)
	funcs["variable_ref"] = itemFunc(runref.KindVariable)
	funcs["service_address"] = serviceAddress
	funcs["service_addresses"] = serviceAddresses
	// Last, with a copy of the functions before it: a template has no
	// templatefile. A template that renders itself would end the process
	// with a stack overflow, which no recover catches.
	funcs["templatefile"] = templateFileFunc(fsys, fileBytes, maps.Clone(funcs))

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
// template syntax of HCL, which sees only vars (design 4.3) and calls only
// funcs.
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

			ctx := &hcl.EvalContext{Variables: attributeValues(vars), Functions: funcs}

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

// runtimeFunc is runtime(reference): a node attribute or another value the
// cluster fills in on the node, such as node.datacenter (design 4.2).
var runtimeFunc = envFunc("reference")

// runtimeEnvFunc is runtime_env(name): an environment variable of the task.
var runtimeEnvFunc = envFunc("name")

// envFunc returns a function that names a value of the environment of the
// task, which holds the node attributes too.
func envFunc(param string) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: param, Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			return cty.StringVal(runref.Part{Kind: runref.KindEnv, Name: args[0].AsString()}.String()), nil
		},
	})
}

// itemFunc returns secret_ref or variable_ref: path, item and options
// { quote, trim }. quote is "conf", "sql" or false; without it the place
// decides (none in a task template, the quoting of the configuration in an
// encoder).
func itemFunc(kind runref.Kind) function.Function {
	return function.New(&function.Spec{
		Params:   []function.Parameter{{Name: "path", Type: cty.String}, {Name: "item", Type: cty.String}},
		VarParam: &function.Parameter{Name: "options", Type: cty.DynamicPseudoType},
		Type:     function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			p := runref.Part{Kind: kind, Name: args[0].AsString(), Item: args[1].AsString()}

			err := options(args[2:], func(name string, v cty.Value) bool {
				switch {
				case name == "trim" && v.Type() == cty.Bool:
					p.Trim = v.True()
				case name == "quote" && v.Type() == cty.Bool && v.False():
					p.Quote = runref.QuoteNone
				case name == "quote" && v.Type() == cty.String && (v.AsString() == "conf" || v.AsString() == "sql"):
					p.Quote = runref.Quote(v.AsString())
				default:
					return false
				}

				return true
			}, `quote ("conf", "sql" or false) and trim (a bool)`)
			if err != nil {
				return cty.NilVal, err
			}

			return cty.StringVal(p.String()), nil
		},
	})
}

// serviceAddress is service_address(name, { scheme, port }): the address of
// one instance of a service of the native catalog, as host:port, or the host
// alone with port = false.
var serviceAddress = function.New(&function.Spec{
	Params:   []function.Parameter{{Name: "name", Type: cty.String}},
	VarParam: &function.Parameter{Name: "options", Type: cty.DynamicPseudoType},
	Type:     function.StaticReturnType(cty.String),
	Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
		p := runref.Part{Kind: runref.KindService, Name: args[0].AsString()}

		err := options(args[1:], func(name string, v cty.Value) bool {
			switch {
			case name == "scheme" && v.Type() == cty.String:
				p.Scheme = v.AsString()
			case name == "port" && v.Type() == cty.Bool:
				p.NoPort = v.False()
			default:
				return false
			}

			return true
		}, "scheme (a string) and port (a bool)")
		if err != nil {
			return cty.NilVal, err
		}

		return cty.StringVal(p.String()), nil
	},
})

// serviceAddresses is service_addresses(name, { scheme, exclude_self }):
// the addresses of all instances of a service. It is a list of one item
// that stands for all of them, so that concat() can put known items before
// it; an encoder writes it as the items of a list.
var serviceAddresses = function.New(&function.Spec{
	Params:   []function.Parameter{{Name: "name", Type: cty.String}},
	VarParam: &function.Parameter{Name: "options", Type: cty.DynamicPseudoType},
	Type:     function.StaticReturnType(cty.List(cty.String)),
	Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
		p := runref.Part{Kind: runref.KindServices, Name: args[0].AsString()}

		err := options(args[1:], func(name string, v cty.Value) bool {
			switch {
			case name == "scheme" && v.Type() == cty.String:
				p.Scheme = v.AsString()
			case name == "exclude_self" && v.Type() == cty.Bool:
				p.ExcludeSelf = v.True()
			default:
				return false
			}

			return true
		}, "scheme (a string) and exclude_self (a bool)")
		if err != nil {
			return cty.NilVal, err
		}

		return cty.ListVal([]cty.Value{cty.StringVal(p.String())}), nil
	},
})

// options reads the options object of a function: set sets one and says
// whether it is one; known lists the options for the error.
func options(args []cty.Value, set func(name string, v cty.Value) bool, known string) error {
	if len(args) == 0 {
		return nil
	}

	if len(args) > 1 || !args[0].Type().IsObjectType() {
		return fmt.Errorf("the options are one object: %s", known)
	}

	for name, v := range args[0].AsValueMap() {
		v, _ = v.Unmark()
		if v.IsNull() || !v.IsKnown() || !set(name, v) {
			return fmt.Errorf("invalid option %s: the options are %s", name, known)
		}
	}

	return nil
}

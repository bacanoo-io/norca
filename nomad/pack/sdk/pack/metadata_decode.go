// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package pack

import (
	"fmt"
	"path"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

// DecodeMetadata decodes the content of a metadata.hcl file. Expressions can
// call file(path) to read a file of the pack, such as the variables schema:
// readFile is called with the cleaned, slash-separated path relative to the
// pack directory, which can't leave the pack.
func DecodeMetadata(filename string, src []byte, readFile func(name string) ([]byte, error)) (*Metadata, error) {
	ctx := &hcl.EvalContext{
		Functions: map[string]function.Function{
			"file": packFileFunc(readFile),
		},
	}
	md := new(Metadata)
	if err := hclsimple.Decode(filename, src, ctx, md); err != nil {
		return nil, err
	}
	return md, nil
}

// CleanPackPath cleans a path relative to a pack directory and rejects
// paths that leave it.
func CleanPackPath(name string) (string, error) {
	p := path.Clean(strings.ReplaceAll(name, "\\", "/"))
	if p == "." || path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, ":") {
		return "", fmt.Errorf("%q is not a file of the pack", name)
	}
	return p, nil
}

func packFileFunc(readFile func(name string) ([]byte, error)) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "path", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			name, err := CleanPackPath(args[0].AsString())
			if err != nil {
				return cty.UnknownVal(cty.String), err
			}
			if readFile == nil {
				return cty.UnknownVal(cty.String), fmt.Errorf("file(%q): files can't be read here", name)
			}
			b, err := readFile(name)
			if err != nil {
				return cty.UnknownVal(cty.String), fmt.Errorf("file(%q): %v", name, err)
			}
			return cty.StringVal(string(b)), nil
		},
	})
}

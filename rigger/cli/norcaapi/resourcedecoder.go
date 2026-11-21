// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/bacanoo-io/norca/rigger/cli/internal/resourcespec"
	"github.com/bacanoo-io/norca/rigger/host"
	"github.com/bacanoo-io/norca/rigger/resource"
)

// ResourceDecoder decodes object kinds and objects with the parsers of
// Norca (the copy in cli/internal/resourcespec): a kind of a module is the
// kind the same file makes with norca object-kind apply and with the pack
// manager, its columns and webhooks in their order.
//
// A module names a resource by the label of its block; the file of Norca
// names a kind by the label and an object by its name attribute. A name
// attribute of the block is the name in Norca's file, else the label is.
type ResourceDecoder struct{}

var _ host.ResourceDecoder = ResourceDecoder{}

// Kinds are object kinds and objects.
func (ResourceDecoder) Kinds() []resource.Kind {
	return []resource.Kind{resource.KindObjectKind, resource.KindObject}
}

// DecodeResource decodes the block in src.
func (ResourceDecoder) DecodeResource(_ context.Context, kind resource.Kind, name string, src []byte) (json.RawMessage, hcl.Diagnostics) {
	f, diags := hclwrite.ParseConfig(src, name+".hcl", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, diags
	}

	if !slices.Contains(ResourceDecoder{}.Kinds(), kind) {
		return nil, decodeDiag(kind, name, fmt.Errorf("a %s is not decoded here", kind))
	}

	blocks := f.Body().Blocks()
	if len(blocks) != 1 {
		return nil, decodeDiag(kind, name, fmt.Errorf("the text holds %d blocks, not one", len(blocks)))
	}

	block := blocks[0]

	label, err := nameOf(block.Body(), name)
	if err != nil {
		return nil, decodeDiag(kind, name, err)
	}

	var spec any

	if kind == resource.KindObjectKind {
		spec, err = objectKind(f, block, label, name)
	} else {
		spec, err = object(block, label, name)
	}

	if err != nil {
		return nil, decodeDiag(kind, name, err)
	}

	data, err := json.Marshal(spec)
	if err != nil {
		return nil, decodeDiag(kind, name, err)
	}

	return data, nil
}

// objectKind decodes an object kind: kind "<Kind>" { group = ..., ... }.
func objectKind(f *hclwrite.File, block *hclwrite.Block, label, name string) (*api.ObjectKind, error) {
	block.Body().RemoveAttribute("name")
	block.SetLabels([]string{label})

	return resourcespec.ParseObjectKindNoFiles(name+".kind.hcl", f.Bytes(), false)
}

// object decodes an object: its attributes and blocks at the top of the
// file.
func object(block *hclwrite.Block, label, name string) (*api.CustomObject, error) {
	out := hclwrite.NewEmptyFile()
	out.Body().SetAttributeValue("name", cty.StringVal(label))

	for n, a := range block.Body().Attributes() {
		if n != "name" {
			out.Body().SetAttributeRaw(n, a.Expr().BuildTokens(nil))
		}
	}

	for _, b := range block.Body().Blocks() {
		out.Body().AppendBlock(b)
	}

	return resourcespec.ParseCustomObjectNoFiles(name+".object.hcl", out.Bytes(), false)
}

// nameOf is the string of the name attribute of body, else name.
func nameOf(body *hclwrite.Body, name string) (string, error) {
	a := body.GetAttribute("name")
	if a == nil {
		return name, nil
	}

	expr, diags := hclsyntax.ParseExpression(a.Expr().BuildTokens(nil).Bytes(), "name", hcl.InitialPos)
	if diags.HasErrors() {
		return "", diags
	}

	v, diags := expr.Value(nil)
	if diags.HasErrors() {
		return "", diags
	}

	if v.Type() != cty.String || v.IsNull() {
		return "", errors.New("name is not a string")
	}

	return v.AsString(), nil
}

func decodeDiag(kind resource.Kind, name string, err error) hcl.Diagnostics {
	return hcl.Diagnostics{{
		Severity: hcl.DiagError, Summary: "Invalid " + string(kind),
		Detail: fmt.Sprintf("The %s %s is not one Norca takes: %s.", kind, name, err),
	}}
}

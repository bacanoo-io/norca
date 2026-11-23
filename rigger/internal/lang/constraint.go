// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package lang

import (
	"fmt"
	"math/big"
	"regexp"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/gocty"
)

// Constraint is a validation block of a variable (design 6.2), or one of
// its attribute and item blocks.
type Constraint struct {
	// Pattern is matched by a string.
	Pattern *regexp.Regexp
	// MinLength and MaxLength bound the length of a string in characters.
	MinLength, MaxLength *int
	// Min and Max bound a number.
	Min, Max *big.Float
	// Integer requires a whole number.
	Integer bool
	// MinItems and MaxItems bound the length of a list, a set or a map.
	MinItems, MaxItems *int
	// Unique requires the items of a list to differ.
	Unique bool
	// UniqueBy requires an attribute of the objects of a list to differ.
	UniqueBy string
	// OneOf are the values a value may have.
	OneOf []cty.Value
	// ItemOf are the values the items of a list may have.
	ItemOf []cty.Value
	// KeyPattern is matched by the keys of a map.
	KeyPattern *regexp.Regexp
	// Attributes constrain attributes of an object, or of the objects of a
	// list.
	Attributes map[string]*Constraint
	// Item constrains every item of a list, a set or a map.
	Item *Constraint
	// Range is the range of the block.
	Range hcl.Range
}

var constraintSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "pattern"},
		{Name: "min_length"},
		{Name: "max_length"},
		{Name: "min"},
		{Name: "max"},
		{Name: "integer"},
		{Name: "min_items"},
		{Name: "max_items"},
		{Name: "unique"},
		{Name: "unique_by"},
		{Name: "one_of"},
		{Name: "item_of"},
		{Name: "key_pattern"},
	},
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "attribute", LabelNames: []string{labelName}},
		{Type: blockItem},
	},
}

// blockItem is the item block of a validation; its other block is
// attribute "<name>".
const blockItem = "item"

// constraintAttrs decodes the attributes of a validation block.
var constraintAttrs = map[string]func(*Constraint, *hcl.Attribute, cty.Value) hcl.Diagnostics{
	"pattern": func(c *Constraint, a *hcl.Attribute, v cty.Value) (d hcl.Diagnostics) {
		c.Pattern, d = asRegexp(a, v)
		return d
	},
	"key_pattern": func(c *Constraint, a *hcl.Attribute, v cty.Value) (d hcl.Diagnostics) {
		c.KeyPattern, d = asRegexp(a, v)
		return d
	},
	"min_length": func(c *Constraint, a *hcl.Attribute, v cty.Value) (d hcl.Diagnostics) {
		c.MinLength, d = asCount(a, v)
		return d
	},
	"max_length": func(c *Constraint, a *hcl.Attribute, v cty.Value) (d hcl.Diagnostics) {
		c.MaxLength, d = asCount(a, v)
		return d
	},
	"min_items": func(c *Constraint, a *hcl.Attribute, v cty.Value) (d hcl.Diagnostics) {
		c.MinItems, d = asCount(a, v)
		return d
	},
	"max_items": func(c *Constraint, a *hcl.Attribute, v cty.Value) (d hcl.Diagnostics) {
		c.MaxItems, d = asCount(a, v)
		return d
	},
	"min": func(c *Constraint, a *hcl.Attribute, v cty.Value) (d hcl.Diagnostics) {
		c.Min, d = asNumber(a, v)
		return d
	},
	"max": func(c *Constraint, a *hcl.Attribute, v cty.Value) (d hcl.Diagnostics) {
		c.Max, d = asNumber(a, v)
		return d
	},
	"integer": func(c *Constraint, a *hcl.Attribute, v cty.Value) (d hcl.Diagnostics) {
		c.Integer, d = asBool(a, v)
		return d
	},
	"unique": func(c *Constraint, a *hcl.Attribute, v cty.Value) (d hcl.Diagnostics) {
		c.Unique, d = asBool(a, v)
		return d
	},
	"unique_by": func(c *Constraint, a *hcl.Attribute, v cty.Value) (d hcl.Diagnostics) {
		c.UniqueBy, d = asString(a, v)
		return d
	},
	"one_of": func(c *Constraint, a *hcl.Attribute, v cty.Value) (d hcl.Diagnostics) {
		c.OneOf, d = asValues(a, v)
		return d
	},
	"item_of": func(c *Constraint, a *hcl.Attribute, v cty.Value) (d hcl.Diagnostics) {
		c.ItemOf, d = asValues(a, v)
		return d
	},
}

// decodeConstraint decodes a validation block, or an attribute or item
// block in one.
func decodeConstraint(block *hcl.Block) (*Constraint, hcl.Diagnostics) {
	content, diags := block.Body.Content(constraintSchema)
	c := &Constraint{Range: block.DefRange}

	for name, attr := range content.Attributes {
		v, moreDiags := attr.Expr.Value(nil)
		diags = append(diags, moreDiags...)

		if moreDiags.HasErrors() {
			continue
		}

		diags = append(diags, constraintAttrs[name](c, attr, v)...)
	}

	for _, b := range content.Blocks {
		// One item block and one attribute block of a name: a second one
		// would replace the first, and with it what the first requires.
		prev, what := c.Item, "The block has an item block"
		if b.Type != blockItem {
			prev, what = c.Attributes[b.Labels[0]], fmt.Sprintf("The attribute %q has a block", b.Labels[0])
		}

		if prev != nil {
			diags = append(diags, errorAt(b.DefRange, "Duplicate "+b.Type+" block",
				fmt.Sprintf("%s already at %s; write the two as one block.", what, prev.Range)))

			continue
		}

		inner, moreDiags := decodeConstraint(b)
		diags = append(diags, moreDiags...)

		if b.Type == blockItem {
			c.Item = inner

			continue
		}

		if c.Attributes == nil {
			c.Attributes = map[string]*Constraint{}
		}

		c.Attributes[b.Labels[0]] = inner
	}

	return c, diags
}

// fits reports where the constraint does not apply to a value of type ty.
func (c *Constraint) fits(ty cty.Type) hcl.Diagnostics {
	if ty == cty.DynamicPseudoType {
		return nil
	}

	var diags hcl.Diagnostics

	for _, f := range c.families() {
		if f.isUsed && !f.accepts(ty) {
			diags = append(diags, errorAt(c.Range, "Validation does not fit the type",
				fmt.Sprintf("%s applies to %s, not to %s.", f.names, f.wants, ty.FriendlyName())))
		}
	}

	return append(diags, c.fitsInner(ty)...)
}

// family is a group of constraints that apply to the same kind of value.
type family struct {
	names   string
	wants   string
	isUsed  bool
	accepts func(cty.Type) bool
}

// families returns the groups of constraints of c and the types each
// accepts.
func (c *Constraint) families() []family {
	return []family{
		{
			names: "pattern, min_length and max_length", wants: "a string",
			isUsed:  c.Pattern != nil || c.MinLength != nil || c.MaxLength != nil,
			accepts: func(ty cty.Type) bool { return ty == cty.String },
		},
		{
			names: "min, max and integer", wants: "a number",
			isUsed:  c.Min != nil || c.Max != nil || c.Integer,
			accepts: func(ty cty.Type) bool { return ty == cty.Number },
		},
		{
			names: "min_items, max_items and item", wants: "a list, a set or a map",
			isUsed:  c.MinItems != nil || c.MaxItems != nil || c.Item != nil,
			accepts: func(ty cty.Type) bool { return elementType(ty) != cty.NilType },
		},
		{
			names: "unique, unique_by and item_of", wants: "a list or a set",
			isUsed:  c.Unique || c.ItemOf != nil || c.UniqueBy != "",
			accepts: isSequence,
		},
		{
			names: "key_pattern", wants: "a map",
			isUsed:  c.KeyPattern != nil,
			accepts: cty.Type.IsMapType,
		},
	}
}

// fitsInner checks the item, unique_by and attribute constraints.
func (c *Constraint) fitsInner(ty cty.Type) hcl.Diagnostics {
	var diags hcl.Diagnostics

	if c.Item != nil && elementType(ty) != cty.NilType {
		diags = append(diags, c.Item.fits(elementType(ty))...)
	}

	object := ty
	if isSequence(ty) {
		object = elementType(ty)
	}

	if c.UniqueBy != "" && isSequence(ty) && !hasAttribute(object, c.UniqueBy) {
		diags = append(diags, errorAt(c.Range, "Validation does not fit the type",
			fmt.Sprintf("unique_by names %q, which the items of %s do not have.", c.UniqueBy, ty.FriendlyName())))
	}

	for name, inner := range c.Attributes {
		if !hasAttribute(object, name) {
			diags = append(diags, errorAt(inner.Range, "Validation does not fit the type",
				fmt.Sprintf("attribute %q is not an attribute of %s.", name, object.FriendlyName())))

			continue
		}

		diags = append(diags, inner.fits(object.AttributeType(name))...)
	}

	return diags
}

func isSequence(ty cty.Type) bool {
	return ty.IsListType() || ty.IsSetType()
}

// elementType returns the type of the items of a list, a set or a map, or
// cty.NilType.
func elementType(ty cty.Type) cty.Type {
	if ty.IsListType() || ty.IsSetType() || ty.IsMapType() {
		return ty.ElementType()
	}

	return cty.NilType
}

func hasAttribute(ty cty.Type, name string) bool {
	return ty.IsObjectType() && ty.HasAttribute(name)
}

func asString(a *hcl.Attribute, v cty.Value) (string, hcl.Diagnostics) {
	var s string
	if err := gocty.FromCtyValue(v, &s); err != nil {
		return "", hcl.Diagnostics{errorAt(a.Expr.Range(), "Invalid value", a.Name+" must be a string.")}
	}

	return s, nil
}

func asBool(a *hcl.Attribute, v cty.Value) (bool, hcl.Diagnostics) {
	var b bool
	if err := gocty.FromCtyValue(v, &b); err != nil {
		return false, hcl.Diagnostics{errorAt(a.Expr.Range(), "Invalid value", a.Name+" must be true or false.")}
	}

	return b, nil
}

func asCount(a *hcl.Attribute, v cty.Value) (*int, hcl.Diagnostics) {
	var n int
	if err := gocty.FromCtyValue(v, &n); err != nil || n < 0 {
		return nil, hcl.Diagnostics{errorAt(a.Expr.Range(), "Invalid value", a.Name+" must be a whole number, 0 or more.")}
	}

	return &n, nil
}

func asNumber(a *hcl.Attribute, v cty.Value) (*big.Float, hcl.Diagnostics) {
	if v.IsNull() || !v.IsKnown() || v.Type() != cty.Number {
		return nil, hcl.Diagnostics{errorAt(a.Expr.Range(), "Invalid value", a.Name+" must be a number.")}
	}

	return v.AsBigFloat(), nil
}

func asRegexp(a *hcl.Attribute, v cty.Value) (*regexp.Regexp, hcl.Diagnostics) {
	s, diags := asString(a, v)
	if diags.HasErrors() {
		return nil, diags
	}

	re, err := regexp.Compile(s)
	if err != nil {
		return nil, hcl.Diagnostics{errorAt(a.Expr.Range(), "Invalid pattern", err.Error()+".")}
	}

	return re, nil
}

func asValues(a *hcl.Attribute, v cty.Value) ([]cty.Value, hcl.Diagnostics) {
	if v.IsNull() || !v.CanIterateElements() || v.Type().IsMapType() || v.Type().IsObjectType() {
		return nil, hcl.Diagnostics{errorAt(a.Expr.Range(), "Invalid value", a.Name+" must be a list.")}
	}

	values := make([]cty.Value, 0, v.LengthInt())
	for it := v.ElementIterator(); it.Next(); {
		_, item := it.Element()
		values = append(values, item)
	}

	return values, nil
}

// errorAt returns an error diagnostic at rng.
func errorAt(rng hcl.Range, summary, detail string) *hcl.Diagnostic {
	return &hcl.Diagnostic{Severity: hcl.DiagError, Summary: summary, Detail: detail, Subject: rng.Ptr()}
}

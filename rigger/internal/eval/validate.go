// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/bacanoo-io/norca-rig/internal/lang"
)

// checker checks a value against the constraints of a variable.
type checker struct {
	rng       hcl.Range
	sensitive bool
	diags     hcl.Diagnostics
}

// fail adds an error about the value at path.
func (k *checker) fail(path, format string, args ...any) {
	k.diags = append(k.diags, &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  "Invalid value",
		Detail:   path + " " + fmt.Sprintf(format, args...) + ".",
		Subject:  k.rng.Ptr(),
	})
}

// show is how a value appears in a message: never a sensitive one.
func (k *checker) show(v cty.Value) string {
	if k.sensitive {
		return "(sensitive)"
	}

	return showValue(v)
}

// check checks v at path against c. A null or unknown value has nothing to
// check.
func (k *checker) check(c *lang.Constraint, v cty.Value, path string) {
	if c == nil || v.IsNull() || !v.IsKnown() {
		return
	}

	v, _ = v.Unmark()
	ty := v.Type()

	switch {
	case ty == cty.String:
		k.checkString(c, v, path)
	case ty == cty.Number:
		k.checkNumber(c, v, path)
	case ty.IsListType() || ty.IsSetType() || ty.IsMapType() || ty.IsTupleType():
		k.checkCollection(c, v, path)
	}

	if c.OneOf != nil && !containsValue(c.OneOf, v) {
		k.fail(path, "is %s, not one of %s", k.show(v), showValues(c.OneOf))
	}

	k.checkAttributes(c, v, path)
}

func (k *checker) checkString(c *lang.Constraint, v cty.Value, path string) {
	s := v.AsString()

	if c.Pattern != nil && !c.Pattern.MatchString(s) {
		k.fail(path, "is %s, which does not match %s", k.show(v), c.Pattern)
	}

	n := utf8.RuneCountInString(s)
	if c.MinLength != nil && n < *c.MinLength {
		k.fail(path, "has %d characters, fewer than %d", n, *c.MinLength)
	}

	if c.MaxLength != nil && n > *c.MaxLength {
		k.fail(path, "has %d characters, more than %d", n, *c.MaxLength)
	}
}

func (k *checker) checkNumber(c *lang.Constraint, v cty.Value, path string) {
	f := v.AsBigFloat()

	if c.Min != nil && f.Cmp(c.Min) < 0 {
		k.fail(path, "is %s, less than %s", k.show(v), c.Min.Text('g', -1))
	}

	if c.Max != nil && f.Cmp(c.Max) > 0 {
		k.fail(path, "is %s, more than %s", k.show(v), c.Max.Text('g', -1))
	}

	if c.Integer && !f.IsInt() {
		k.fail(path, "is %s, not a whole number", k.show(v))
	}
}

func (k *checker) checkCollection(c *lang.Constraint, v cty.Value, path string) {
	n := v.LengthInt()

	if c.MinItems != nil && n < *c.MinItems {
		k.fail(path, "has %d items, fewer than %d", n, *c.MinItems)
	}

	if c.MaxItems != nil && n > *c.MaxItems {
		k.fail(path, "has %d items, more than %d", n, *c.MaxItems)
	}

	seen := map[string]string{}

	for it := v.ElementIterator(); it.Next(); {
		key, item := it.Element()
		itemPath := path + index(key)

		k.checkItem(c, key, item, itemPath, path, seen)
	}
}

// checkItem checks one item of a collection: its key, its place among the
// others, and its own constraints.
func (k *checker) checkItem(c *lang.Constraint, key, item cty.Value, itemPath, path string, seen map[string]string) {
	if c.KeyPattern != nil && key.Type() == cty.String && !c.KeyPattern.MatchString(key.AsString()) {
		k.fail(path, "has the key %q, which does not match %s", key.AsString(), c.KeyPattern)
	}

	if c.ItemOf != nil && !containsValue(c.ItemOf, item) {
		k.fail(itemPath, "is %s, not one of %s", k.show(item), showValues(c.ItemOf))
	}

	if c.Unique {
		k.unique(seen, showValue(item), itemPath)
	}

	if c.UniqueBy != "" && item.Type().IsObjectType() && item.Type().HasAttribute(c.UniqueBy) {
		k.unique(seen, showValue(item.GetAttr(c.UniqueBy)), itemPath+"."+c.UniqueBy)
	}

	k.check(c.Item, item, itemPath)

	if c.Attributes != nil && item.Type().IsObjectType() {
		k.checkAttributes(c, item, itemPath)
	}
}

// unique fails when shown was seen before at another path.
func (k *checker) unique(seen map[string]string, shown, path string) {
	if first, dup := seen[shown]; dup {
		k.fail(path, "is the same as %s", first)

		return
	}

	seen[shown] = path
}

// checkAttributes checks the attributes of an object.
func (k *checker) checkAttributes(c *lang.Constraint, v cty.Value, path string) {
	if c.Attributes == nil || !v.Type().IsObjectType() {
		return
	}

	for name, inner := range c.Attributes {
		if v.Type().HasAttribute(name) {
			k.check(inner, v.GetAttr(name), path+"."+name)
		}
	}
}

func containsValue(values []cty.Value, v cty.Value) bool {
	for _, candidate := range values {
		if eq := candidate.Equals(v); eq.IsKnown() && eq.True() {
			return true
		}
	}

	return false
}

// index returns the index expression of a key: [1] or ["name"].
func index(key cty.Value) string {
	if key.Type() == cty.String {
		return "[" + strconv.Quote(key.AsString()) + "]"
	}

	return "[" + key.AsBigFloat().Text('f', -1) + "]"
}

// showValue returns a short text of a value for a message.
func showValue(v cty.Value) string {
	v, _ = v.Unmark()

	switch {
	case v.IsNull():
		return "null"
	case !v.IsKnown():
		return "(known after evaluation)"
	case v.Type() == cty.String:
		return strconv.Quote(v.AsString())
	case v.Type() == cty.Number:
		return v.AsBigFloat().Text('g', -1)
	case v.Type() == cty.Bool:
		return strconv.FormatBool(v.True())
	default:
		return v.GoString()
	}
}

func showValues(values []cty.Value) string {
	shown := make([]string, len(values))
	for i, v := range values {
		shown[i] = showValue(v)
	}

	return strings.Join(shown, ", ")
}

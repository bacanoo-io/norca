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

	"github.com/bacanoo-io/norca/rigger/internal/lang"
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
		Summary:  summaryInvalidValue,
		Detail:   path + " " + fmt.Sprintf(format, args...) + ".",
		Subject:  k.rng.Ptr(),
	})
}

// hidden stands for a sensitive value in a message.
const hidden = "(sensitive)"

// show is how a value appears in a message: never a sensitive one, whether
// the variable says so or the value it was given.
func (k *checker) show(v cty.Value) string {
	if k.sensitive || v.ContainsMarked() {
		return hidden
	}

	return showValue(v)
}

// check checks v at path against c. A null or unknown value has nothing to
// check.
func (k *checker) check(c *lang.Constraint, v cty.Value, path string) {
	if c == nil || v.IsNull() || !v.IsKnown() {
		return
	}

	if v.IsMarked() {
		// What is in a sensitive value is sensitive, the keys of a map too.
		outer := k.sensitive
		k.sensitive = true

		defer func() { k.sensitive = outer }()
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
	// A set with an unknown item has no known size, and no known items.
	if v.Type().IsSetType() && !v.IsWhollyKnown() {
		return
	}

	n := v.LengthInt()

	if c.MinItems != nil && n < *c.MinItems {
		k.fail(path, "has %d items, fewer than %d", n, *c.MinItems)
	}

	if c.MaxItems != nil && n > *c.MaxItems {
		k.fail(path, "has %d items, more than %d", n, *c.MaxItems)
	}

	seen := map[string]string{}
	place := 0

	for it := v.ElementIterator(); it.Next(); place++ {
		key, item := it.Element()
		itemPath := path + k.index(key, place)

		k.checkItem(c, key, item, itemPath, path, seen)
	}
}

// checkItem checks one item of a collection: its key, its place among the
// others, and its own constraints.
func (k *checker) checkItem(c *lang.Constraint, key, item cty.Value, itemPath, path string, seen map[string]string) {
	if c.KeyPattern != nil && key.Type() == cty.String && !c.KeyPattern.MatchString(key.AsString()) {
		k.fail(path, "has the key %s, which does not match %s", k.show(key), c.KeyPattern)
	}

	if c.ItemOf != nil && !containsValue(c.ItemOf, item) {
		k.fail(itemPath, "is %s, not one of %s", k.show(item), showValues(c.ItemOf))
	}

	// An item not known yet is not the same as another one.
	if c.Unique && item.IsWhollyKnown() {
		k.unique(seen, showValue(item), itemPath)
	}

	if by, ok := attribute(item, c.UniqueBy); ok && by.IsWhollyKnown() {
		k.unique(seen, showValue(by), itemPath+"."+c.UniqueBy)
	}

	k.check(c.Item, item, itemPath)
	k.checkAttributes(c, item, itemPath)
}

// attribute returns the attribute name of v, when v is an object that has
// it. An object that is null or not known yet has no attributes to read.
func attribute(v cty.Value, name string) (cty.Value, bool) {
	if name == "" || v.IsNull() || !v.IsKnown() || !v.Type().IsObjectType() || !v.Type().HasAttribute(name) {
		return cty.NilVal, false
	}

	return v.GetAttr(name), true
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
	for name, inner := range c.Attributes {
		if attr, ok := attribute(v, name); ok {
			k.check(inner, attr, path+"."+name)
		}
	}
}

func containsValue(values []cty.Value, v cty.Value) bool {
	v, _ = v.UnmarkDeep() // a comparison with a marked value is marked

	for _, candidate := range values {
		if eq := candidate.Equals(v); eq.IsKnown() && eq.True() {
			return true
		}
	}

	return false
}

// index returns the index expression of a key: [1] or ["name"], the name
// hidden in a sensitive map. The key of an item of a set is the item: one
// that is neither a string nor a number is named by its place in the order
// of the set.
func (k *checker) index(key cty.Value, place int) string {
	if inner, _ := key.Unmark(); !inner.IsNull() && inner.IsKnown() {
		switch inner.Type() {
		case cty.String:
			return "[" + k.show(key) + "]"
		case cty.Number:
			return "[" + inner.AsBigFloat().Text('f', -1) + "]"
		}
	}

	return "[" + strconv.Itoa(place) + "]"
}

// showValue returns a short text of a value for a message.
func showValue(v cty.Value) string {
	v, _ = v.Unmark()

	switch {
	case v.IsNull():
		return nullText
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

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"slices"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/bacanoo-io/norca/rigger/internal/lang"
)

// A set with an unknown item has no known size: there is nothing to check
// until it is known.
func TestChecker_UnknownInSet(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic: %v", r)
		}
	}()

	one := 1
	k := &checker{}
	k.check(&lang.Constraint{MinItems: &one}, cty.SetVal([]cty.Value{cty.UnknownVal(cty.String)}), "var.s")

	if k.diags.HasErrors() {
		t.Errorf("check() = %v, want nothing of a value not known", k.diags)
	}
}

// nameAttr is the attribute of the objects of the tests.
const nameAttr = "name"

// named is an object with a name, an item of the collections of the tests.
func named(name cty.Value) cty.Value {
	return cty.ObjectVal(map[string]cty.Value{nameAttr: name})
}

// checked returns the details of the errors of v checked against c.
func checked(c *lang.Constraint, v cty.Value) []string {
	k := &checker{}
	k.check(c, v, "var.x")

	details := make([]string, len(k.diags))
	for i, d := range k.diags {
		details[i] = d.Detail
	}

	return details
}

// An item of a set has no index and no key. A string or a number names
// itself; any other item is named by its place in the order of the set.
func TestChecker_SetItems(t *testing.T) {
	t.Parallel()

	one, two := 1, 2

	tests := []struct {
		name     string
		c        *lang.Constraint
		v        cty.Value
		expected []string
	}{
		{
			name: "objects, enough of them",
			c:    &lang.Constraint{MinItems: &one},
			v:    cty.SetVal([]cty.Value{named(cty.StringVal("a"))}),
		},
		{
			name:     "objects, too few of them",
			c:        &lang.Constraint{MinItems: &two},
			v:        cty.SetVal([]cty.Value{named(cty.StringVal("a"))}),
			expected: []string{"var.x has 1 items, fewer than 2."},
		},
		{
			name:     "an object that does not fit",
			c:        &lang.Constraint{Attributes: map[string]*lang.Constraint{nameAttr: {MinLength: &two}}},
			v:        cty.SetVal([]cty.Value{named(cty.StringVal("a"))}),
			expected: []string{"var.x[0].name has 1 characters, fewer than 2."},
		},
		{
			name:     "bools",
			c:        &lang.Constraint{ItemOf: []cty.Value{cty.False}},
			v:        cty.SetVal([]cty.Value{cty.True, cty.False}),
			expected: []string{"var.x[1] is true, not one of false."},
		},
		{
			name:     "lists",
			c:        &lang.Constraint{Item: &lang.Constraint{MaxItems: &one}},
			v:        cty.SetVal([]cty.Value{cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")})}),
			expected: []string{"var.x[0] has 2 items, more than 1."},
		},
		{
			name:     "strings",
			c:        &lang.Constraint{ItemOf: []cty.Value{cty.StringVal("a")}},
			v:        cty.SetVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")}),
			expected: []string{`var.x["b"] is "b", not one of "a".`},
		},
		{
			name:     "numbers",
			c:        &lang.Constraint{ItemOf: []cty.Value{cty.NumberIntVal(5)}},
			v:        cty.SetVal([]cty.Value{cty.NumberIntVal(5), cty.NumberIntVal(7)}),
			expected: []string{"var.x[7] is 7, not one of 5."},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := checked(tt.c, tt.v); !slices.Equal(got, tt.expected) {
				t.Errorf("check() = %q, want %q", got, tt.expected)
			}
		})
	}
}

// An item that is null has no attributes, and one that is not known yet is
// not the same as another: neither is compared or checked.
func TestChecker_ItemsWithoutAttributes(t *testing.T) {
	t.Parallel()

	two := 2
	ty := cty.Object(map[string]cty.Type{nameAttr: cty.String})
	a := named(cty.StringVal("a"))

	tests := []struct {
		name     string
		c        *lang.Constraint
		v        cty.Value
		expected []string
	}{
		{
			name: "unique_by with null items",
			c:    &lang.Constraint{UniqueBy: nameAttr},
			v:    cty.ListVal([]cty.Value{a, cty.NullVal(ty), cty.NullVal(ty)}),
		},
		{
			name:     "unique_by with the same name twice",
			c:        &lang.Constraint{UniqueBy: nameAttr},
			v:        cty.ListVal([]cty.Value{a, cty.NullVal(ty), a}),
			expected: []string{"var.x[2].name is the same as var.x[0].name."},
		},
		{
			name:     "attributes with a null item",
			c:        &lang.Constraint{Attributes: map[string]*lang.Constraint{nameAttr: {MinLength: &two}}},
			v:        cty.ListVal([]cty.Value{cty.NullVal(ty), a}),
			expected: []string{"var.x[1].name has 1 characters, fewer than 2."},
		},
		{
			name: "unique_by with items not known",
			c:    &lang.Constraint{UniqueBy: nameAttr},
			v:    cty.ListVal([]cty.Value{cty.UnknownVal(ty), cty.UnknownVal(ty)}),
		},
		{
			name: "unique_by with names not known",
			c:    &lang.Constraint{UniqueBy: nameAttr},
			v:    cty.ListVal([]cty.Value{named(cty.UnknownVal(cty.String)), named(cty.UnknownVal(cty.String))}),
		},
		{
			name: "unique with items not known",
			c:    &lang.Constraint{Unique: true},
			v:    cty.ListVal([]cty.Value{cty.UnknownVal(cty.String), cty.UnknownVal(cty.String)}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := checked(tt.c, tt.v); !slices.Equal(got, tt.expected) {
				t.Errorf("check() = %q, want %q", got, tt.expected)
			}
		})
	}
}

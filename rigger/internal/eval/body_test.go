// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval_test

import (
	"strings"
	"testing"
)

// The blocks of one type in a body are an object by label, or a list when
// they have no label. A block that has no place of its own there is an
// error: one with the labels of another block, or with another number of
// labels than the others. It is not dropped.
func TestEvaluate_BlockLabels(t *testing.T) {
	t.Parallel()

	const (
		a  = "  thing \"a\" {\n    v = 1\n  }\n"
		b  = "  thing \"b\" {\n    v = 2\n  }\n"
		ax = "  thing \"a\" \"x\" {\n    v = 3\n  }\n"
		ay = "  thing \"a\" \"y\" {\n    v = 4\n  }\n"
		no = "  thing {\n    v = 5\n  }\n"
	)

	dynamic := func(forEach, labels string) string {
		return "  dynamic \"thing\" {\n    for_each = " + forEach + "\n    labels   = " + labels + "\n\n    content {\n      v = thing.value\n    }\n  }\n"
	}

	tests := []struct {
		name     string
		body     string
		spec     string // the spec of the object
		expected string // in the diagnostics
	}{
		{name: "a label", body: a + b, spec: `{"thing":{"a":{"v":1},"b":{"v":2}}}`},
		{name: "two labels", body: ax + ay, spec: `{"thing":{"a":{"x":{"v":3},"y":{"v":4}}}}`},
		{name: "no label", body: no + no, spec: `{"thing":[{"v":5},{"v":5}]}`},
		{
			name: "the same label twice", body: a + b + a,
			expected: `x.object.hcl:8,3-12: Duplicate block; The block thing "a" is in one body twice.`,
		},
		{
			name: "the same two labels twice", body: ax + ay + ax,
			expected: `x.object.hcl:8,3-16: Duplicate block; The block thing "a" "x" is in one body twice.`,
		},
		{
			name: "fewer labels than the first block", body: ax + a,
			expected: "x.object.hcl:5,3-12: Blocks with different labels; The thing blocks of one body have the same number of labels: " +
				"this one has 1, and the one at x.object.hcl:2,3-16 has 2.",
		},
		{
			name: "more labels than the first block", body: a + ax,
			expected: "x.object.hcl:5,3-16: Blocks with different labels; The thing blocks of one body have the same number of labels: " +
				"this one has 2, and the one at x.object.hcl:2,3-12 has 1.",
		},
		{
			name: "a label after no label", body: no + a,
			expected: "x.object.hcl:5,3-12: Blocks with different labels; The thing blocks of one body have the same number of labels: " +
				"this one has 1, and the one at x.object.hcl:2,3-8 has 0.",
		},
		{name: "dynamic blocks", body: dynamic(`["a", "b"]`, "[thing.value]"), spec: `{"thing":{"a":{"v":"a"},"b":{"v":"b"}}}`},
		{
			name: "dynamic blocks with the same label", body: dynamic(`["a", "a"]`, "[thing.value]"),
			expected: `x.object.hcl:2,3-18: Duplicate block; The block thing "a" is in one body twice.`,
		},
		{
			name: "a dynamic block with the label of a block", body: a + dynamic(`["a"]`, "[thing.value]"),
			expected: `x.object.hcl:5,3-18: Duplicate block; The block thing "a" is in one body twice.`,
		},
		{name: "a number as the label of a dynamic block", body: dynamic(`["a", "b"]`, "[thing.key]"), spec: `{"thing":{"0":{"v":"a"},"1":{"v":"b"}}}`},
		{
			name: "null as the label of a dynamic block", body: dynamic(`["a"]`, "[null]"),
			expected: "x.object.hcl:4,16-22: Invalid dynamic block; The labels of a dynamic block are a list of strings, the labels of each block it makes.",
		},
		{
			name: "a list as the label of a dynamic block", body: dynamic(`["a"]`, "[[thing.value]]"),
			expected: "Invalid dynamic block; The labels of a dynamic block are a list of strings",
		},
		{
			name: "labels of a dynamic block that are no list", body: dynamic(`["a"]`, "thing.value"),
			expected: "Invalid dynamic block; The labels of a dynamic block are a list of strings",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res, diags := evaluate(t, module(map[string]string{xObject: "object \"x\" {\n" + tt.body + "}\n"}), ``)

			if tt.expected != "" {
				if got := diags.Error(); len(diags) != 1 || !strings.Contains(got, tt.expected) {
					t.Errorf("diagnostics = %q, want the one that contains %q", got, tt.expected)
				}

				return
			}

			if diags.HasErrors() {
				t.Fatal(diags)
			}

			if got := string(res.Resources[0].Spec); got != tt.spec {
				t.Errorf("spec = %s, want %s", got, tt.spec)
			}
		})
	}
}

// The rules block of an ACL policy is a document of the host, written out
// as text: how its blocks are labelled is for the host to judge. Two blocks
// of one label are both in the text.
func TestEvaluate_BlockLabelsOfDocument(t *testing.T) {
	t.Parallel()

	res, diags := evaluate(t, module(map[string]string{
		xACL: "policy \"x\" {\n  rules {\n    namespace \"a\" {\n      policy = \"read\"\n    }\n\n    namespace \"a\" {\n      capabilities = [\"submit-job\"]\n    }\n  }\n}\n",
	}), ``)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	spec := string(res.Resources[0].Spec)
	for _, want := range []string{`policy = \"read\"`, `capabilities = [\"submit-job\"]`} {
		if !strings.Contains(spec, want) {
			t.Errorf("spec = %s, want it to contain %s", spec, want)
		}
	}
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package version_test

import (
	"errors"
	"testing"

	"github.com/bacanoo-io/norca/rigger/version"
)

// Versions the tests use more than once.
const (
	v100 = "1.0.0"
	v123 = "1.2.3"
)

func mustParse(t *testing.T, s string) version.Version {
	t.Helper()

	v, err := version.Parse(s)
	if err != nil {
		t.Fatal(err)
	}

	return v
}

// Parse takes the versions of Semantic Versioning 2.0.0, with a leading v
// or without, and writes them back as they were (but the v).
func TestParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		text     string
		expected string
	}{
		{name: "a version", text: v123, expected: v123},
		{name: "a leading v", text: "v1.2.3", expected: v123},
		{name: "zeros", text: "0.0.0", expected: "0.0.0"},
		{name: "a pre-release", text: "1.2.3-rc.1", expected: "1.2.3-rc.1"},
		{name: "a zero as an identifier", text: "1.2.3-0", expected: "1.2.3-0"},
		{name: "a word that starts with a zero", text: "1.2.3-0a.00x", expected: "1.2.3-0a.00x"},
		{name: "dashes in an identifier", text: "1.2.3-alpha-1.x-y.-", expected: "1.2.3-alpha-1.x-y.-"},
		{name: "a number of more than 64 bits as an identifier", text: "1.0.0-99999999999999999999", expected: "1.0.0-99999999999999999999"},
		{name: "build metadata", text: "1.2.3+build.5", expected: "1.2.3+build.5"},
		{name: "build metadata that starts with zeros", text: "1.2.3+001", expected: "1.2.3+001"},
		{name: "a dash in build metadata", text: "1.2.3+a-b", expected: "1.2.3+a-b"},
		{name: "a pre-release and build metadata", text: "v1.2.3-rc.1+exp.sha.5114f85", expected: "1.2.3-rc.1+exp.sha.5114f85"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v, err := version.Parse(tt.text)
			if err != nil {
				t.Fatalf("Parse(%q) = %v", tt.text, err)
			}

			if got := v.String(); got != tt.expected {
				t.Errorf("Parse(%q) = %s, want %s", tt.text, got, tt.expected)
			}
		})
	}
}

// A text that is not a version is none for Parse, which is the one check
// of the versions of an index, of the tags of a repository and of an OCI
// registry.
func TestParse_Invalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
	}{
		{name: "empty", text: ""},
		{name: "a word", text: "x"},
		{name: "two numbers", text: "1.2"},
		{name: "four numbers", text: "1.2.3.4"},
		{name: "a number that starts with a zero", text: "01.2.3"},
		{name: "a sign", text: "+1.2.3"},
		{name: "a negative number", text: "1.2.-3"},
		{name: "a space before", text: " 1.2.3"},
		{name: "two leading v", text: "vv1.2.3"},
		{name: "an empty pre-release", text: "1.2.3-"},
		{name: "an empty pre-release before build metadata", text: "1.2.3-+b"},
		{name: "an empty identifier", text: "1.2.3-a..b"},
		{name: "an identifier that ends with a dot", text: "1.2.3-a."},
		{name: "a numeric identifier that starts with a zero", text: "1.2.3-01"},
		{name: "a space in an identifier", text: "1.2.3-a b"},
		{name: "a newline after", text: "1.2.3-rc.1\n"},
		{name: "a letter that is not ASCII", text: "1.2.3-é"},
		{name: "an underscore", text: "1.2.3-rc_1"},
		{name: "a path as a pre-release", text: "1.2.3-../../x"},
		{name: "empty build metadata", text: "1.2.3+"},
		{name: "build metadata twice", text: "1.2.3+a+b"},
		{name: "an empty identifier of build metadata", text: "1.2.3+a..b"},
		{name: "a path as build metadata", text: "1.2.3+../x"},
		{name: "a newline in build metadata", text: "1.2.3+b\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v, err := version.Parse(tt.text)
			if !errors.Is(err, version.ErrInvalid) {
				t.Errorf("Parse(%q) = %q, %v, want an invalid version", tt.text, v.String(), err)
			}
		})
	}
}

// The identifiers of a pre-release: numbers by value, however long, and
// before words; words by their bytes.
func TestCompare_PreRelease(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		a, b     version.Version
		expected int
	}{
		{name: "numbers by value", a: mustParse(t, "1.0.0-2"), b: mustParse(t, "1.0.0-10"), expected: -1},
		{
			name: "numbers of more than 64 bits by value",
			a:    mustParse(t, "1.0.0-99999999999999999999"), b: mustParse(t, "1.0.0-100000000000000000000"), expected: -1,
		},
		{
			name: "numbers of more than 64 bits of one length",
			a:    mustParse(t, "1.0.0-100000000000000000001"), b: mustParse(t, "1.0.0-100000000000000000000"), expected: 1,
		},
		{
			name: "the same number of more than 64 bits",
			a:    mustParse(t, "1.0.0-rc.99999999999999999999"), b: mustParse(t, "1.0.0-rc.99999999999999999999"),
		},
		{name: "a number before a word", a: mustParse(t, "1.0.0-99999999999999999999"), b: mustParse(t, "1.0.0-a"), expected: -1},
		{name: "a word after a number", a: mustParse(t, "1.0.0-0a"), b: mustParse(t, "1.0.0-1"), expected: 1},
		{name: "words by their bytes", a: mustParse(t, "1.0.0-alpha"), b: mustParse(t, "1.0.0-beta"), expected: -1},
		{
			name: "zeros before a number built in code are no other number",
			a:    version.Version{Major: 1, Pre: []string{"01"}}, b: version.Version{Major: 1, Pre: []string{"1"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := version.Compare(tt.a, tt.b); got != tt.expected {
				t.Errorf("Compare(%s, %s) = %d, want %d", tt.a, tt.b, got, tt.expected)
			}

			if got := version.Compare(tt.b, tt.a); got != -tt.expected {
				t.Errorf("Compare(%s, %s) = %d, want %d", tt.b, tt.a, got, -tt.expected)
			}
		})
	}
}

func TestCompare(t *testing.T) {
	t.Parallel()

	order := []string{"0.9.9", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", v100, "1.0.1", "1.2.0", "10.0.0"}

	for i := range len(order) - 1 {
		a, b := mustParse(t, order[i]), mustParse(t, order[i+1])
		if version.Compare(a, b) >= 0 || version.Compare(b, a) <= 0 {
			t.Errorf("%s is not before %s", a, b)
		}
	}

	for _, bad := range []string{"1.2", "01.2.3", "1.2.3-", "x", ""} {
		if _, err := version.Parse(bad); err == nil {
			t.Errorf("Parse(%q) took an invalid version", bad)
		}
	}
}

func TestConstraint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		constraint string
		yes, no    []string
	}{
		{"~> 1.2", []string{"1.2.0", "1.9.9"}, []string{"1.1.9", "2.0.0", "1.3.0-rc.1"}},
		{"~> 1.2.3", []string{v123, "1.2.9"}, []string{"1.3.0", "1.2.2"}},
		{"~> 1", []string{v100, "1.9.0"}, []string{"2.0.0"}},
		{">= 1.0, < 2", []string{v100, "1.99.0"}, []string{"0.9.0", "2.0.0"}},
		{v123, []string{v123}, []string{"1.2.4"}},
		{"!= 1.2.3, >= 1.2.0", []string{"1.2.4"}, []string{v123}},
		{">= 1.0.0-rc.1", []string{"1.0.0-rc.2", v100}, []string{"1.0.0-beta.1"}},
	}

	for _, tt := range tests {
		c, err := version.ParseConstraint(tt.constraint)
		if err != nil {
			t.Fatalf("%s: %v", tt.constraint, err)
		}

		for _, s := range tt.yes {
			if !c.Check(mustParse(t, s)) {
				t.Errorf("%s does not take %s", tt.constraint, s)
			}
		}

		for _, s := range tt.no {
			if c.Check(mustParse(t, s)) {
				t.Errorf("%s takes %s", tt.constraint, s)
			}
		}
	}

	c, _ := version.ParseConstraint("~> 0.3.0")
	if v, ok := c.Newest([]version.Version{mustParse(t, "0.3.0"), mustParse(t, "0.4.0"), mustParse(t, "0.3.5")}); !ok || v.String() != "0.3.5" {
		t.Errorf("Newest = %s, want 0.3.5", v)
	}

	for _, bad := range []string{"~>", ">= x", "> 1.2"} {
		if _, err := version.ParseConstraint(bad); err == nil {
			t.Errorf("ParseConstraint(%q) took an invalid constraint", bad)
		}
	}
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package version_test

import (
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

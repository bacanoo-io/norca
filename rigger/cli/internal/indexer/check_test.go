// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package indexer

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/rigger/index"
)

// A listed version is its source in everything the index says of it: also
// in what a catalog shows and filters on.
func TestCompare(t *testing.T) {
	t.Parallel()

	source := func() index.Version {
		return index.Version{
			Version: "1.0.0", Tag: "greeter/v1.0.0", Commit: "0123456789abcdef0123456789abcdef01234567", Digest: "sha256:aa",
			Icon: "sha256:bb", Requires: &index.Requires{Norca: ">= 0.3.0"}, Modules: []index.LockedModule{{Source: "acme/lib", Version: "1.0.0"}},
			Description: "Says hello", Category: "application", Keywords: []string{"greeting", "demo"}, License: "MPL-2.0",
		}
	}

	tests := []struct {
		name string
		// change makes the listed version differ from its source.
		change func(v *index.Version)
		// expected is in the problem; empty for none.
		expected string
	}{
		{name: "as the source", change: func(*index.Version) {}},
		{name: "the day it was listed is not of the source", change: func(v *index.Version) { v.Listed = "2026-11-18" }},
		{name: "commit", change: func(v *index.Version) { v.Commit = strings.Repeat("f", 40) }, expected: "the tag greeter/v1.0.0 points to"},
		{name: "digest", change: func(v *index.Version) { v.Digest = "sha256:cc" }, expected: "the digest of the source is sha256:aa"},
		{name: "icon", change: func(v *index.Version) { v.Icon = "" }, expected: "the icon of the source"},
		{name: "requires", change: func(v *index.Version) { v.Requires = nil }, expected: "the requirements are not those of the source"},
		{name: "modules", change: func(v *index.Version) { v.Modules = nil }, expected: "the modules are not those"},
		{
			name: "description", change: func(v *index.Version) { v.Description = "Something else entirely" },
			expected: `the description of the source is "Says hello", the entry lists "Something else entirely"`,
		},
		{
			name: "category", change: func(v *index.Version) { v.Category = "database" },
			expected: `the category of the source is "application", the entry lists "database"`,
		},
		{
			name: "a keyword more", change: func(v *index.Version) { v.Keywords = append(v.Keywords, "official") },
			expected: `the keywords of the source are ["greeting" "demo"], the entry lists ["greeting" "demo" "official"]`,
		},
		{name: "no keywords", change: func(v *index.Version) { v.Keywords = nil }, expected: "the keywords of the source are"},
		{
			name: "license", change: func(v *index.Version) { v.License = "Proprietary" },
			expected: `the license of the source is "MPL-2.0", the entry lists "Proprietary"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, listed := source(), source()
			tt.change(&listed)

			var problems []string

			compare("modules/a/acme/greeter.json 1.0.0", &got, &listed, func(format string, args ...any) {
				problems = append(problems, fmt.Sprintf(format, args...))
			})

			switch {
			case tt.expected == "" && len(problems) > 0:
				t.Errorf("compare() reports %q, want nothing", problems)
			case tt.expected != "" && (len(problems) != 1 || !strings.Contains(problems[0], tt.expected)):
				t.Errorf("compare() reports %q, want one problem with %q", problems, tt.expected)
			}
		})
	}
}

// A source without keywords is its entry whether the entry has no keywords
// or an empty list of them.
func TestCompare_NoKeywords(t *testing.T) {
	t.Parallel()

	got := index.Version{Commit: "c", Digest: "d"}
	listed := index.Version{Commit: "c", Digest: "d", Keywords: []string{}}

	compare("entry 1.0.0", &got, &listed, func(format string, args ...any) {
		t.Errorf("compare() reports "+format, args...)
	})
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package lang_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bacanoo-io/norca/rigger/internal/lang"
)

// The version of a metadata.hcl is one the rest of Rigger reads: what
// version.Parse takes, written without a leading v, as a tag, the index and
// the lock file write it. A version only the metadata took could be
// written and never published, listed or met by a constraint.
func TestLoad_MetadataVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		version string
		invalid bool
	}{
		{name: "a version", version: "1.2.0"},
		{name: "a pre-release", version: "1.2.0-rc.1"},
		{name: "a pre-release and build metadata", version: "1.2.0-rc.1+build.5"},
		{name: "one number", version: "1", invalid: true},
		{name: "a leading v", version: "v1.2.0", invalid: true},
		{name: "a number that starts with a zero", version: "1.02.0", invalid: true},
		{name: "an empty identifier of a pre-release", version: "1.2.0-a..b", invalid: true},
		{name: "a numeric identifier that starts with a zero", version: "1.2.0-01", invalid: true},
		{name: "a pre-release of dots", version: "1.2.0-..", invalid: true},
		{name: "an empty identifier of build metadata", version: "1.2.0+a..b", invalid: true},
		{name: "a number of more than 64 bits", version: "99999999999999999999.0.0", invalid: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fsys := fstest.MapFS{metadataHCL: {Data: []byte("module {\n  name    = \"app\"\n  version = \"" + tt.version + "\"\n}\n")}}

			m, diags := lang.Load(t.Context(), fsys, limits)

			if !tt.invalid {
				if diags.HasErrors() {
					t.Fatalf("Load() = %s, want the version %s taken", diags.Error(), tt.version)
				}

				if m.Metadata.Version != tt.version {
					t.Errorf("version = %q, want %q", m.Metadata.Version, tt.version)
				}

				return
			}

			if expected := "Invalid version"; !strings.Contains(diags.Error(), expected) {
				t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), expected)
			}
		})
	}
}

// requires.norca is a constraint of Rigger, which the servers check against
// their version: one ParseConstraint does not take is refused at the load,
// not at the first install.
func TestLoad_MetadataRequiresNorca(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		constraint string
		invalid    bool
	}{
		{name: "a lower bound", constraint: ">= 0.3.0"},
		{name: "a pessimistic constraint", constraint: "~> 0.3"},
		{name: "two conditions", constraint: ">= 0.3.0, < 1.0.0"},
		{name: "no operator", constraint: "0.3.0"},
		{name: "a word", constraint: "latest", invalid: true},
		{name: "conditions without a comma", constraint: ">= 0.3.0 < 1.0.0", invalid: true},
		{name: "an empty condition", constraint: ">= 0.3.0,", invalid: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fsys := fstest.MapFS{metadataHCL: {Data: []byte("module {\n  name    = \"app\"\n  version = \"1.0.0\"\n" +
				"  requires {\n    norca = \"" + tt.constraint + "\"\n  }\n}\n")}}

			m, diags := lang.Load(t.Context(), fsys, limits)

			if !tt.invalid {
				if diags.HasErrors() {
					t.Fatalf("Load() = %s, want requires.norca %q taken", diags.Error(), tt.constraint)
				}

				if m.Metadata.Requires.Norca != tt.constraint {
					t.Errorf("requires.norca = %q, want %q", m.Metadata.Requires.Norca, tt.constraint)
				}

				return
			}

			if expected := "Invalid Norca requirement"; !strings.Contains(diags.Error(), expected) {
				t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), expected)
			}
		})
	}
}

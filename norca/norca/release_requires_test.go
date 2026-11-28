// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// requires.norca of a module is a constraint of Rigger: the servers read it
// as the rigger program reads the required_version of a stack, so a module
// a stack takes is not refused by the servers for the same version.
func TestRelease_RequiresNorca(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name       string
		constraint string
		server     string
		err        string
	}{
		{name: "no requirement", server: "0.3.0"},
		{name: "a lower bound met", constraint: ">= 0.3.0", server: "0.3.0"},
		{name: "a short lower bound met", constraint: ">= 0.3", server: "0.3.1"},
		{name: "a pessimistic constraint met", constraint: "~> 0.2", server: "0.3.0"},
		{name: "a pessimistic constraint not met", constraint: "~> 0.2.1", server: "0.3.0",
			err: "the module requires Norca ~> 0.2.1, the servers run 0.3.0"},
		{name: "a lower bound not met", constraint: ">= 1.0.0", server: "0.3.0",
			err: "the module requires Norca >= 1.0.0, the servers run 0.3.0"},
		{name: "a pre-release of the servers", constraint: ">= 0.3.0", server: "0.4.0-dev",
			err: "the module requires Norca >= 0.3.0, the servers run 0.4.0-dev"},
		{name: "a pre-release named", constraint: ">= 0.4.0-dev", server: "0.4.0-dev"},
		{name: "not a constraint", constraint: "latest", server: "0.3.0",
			err: `invalid Norca version requirement "latest"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkReleaseRequirementsAt(&structs.ReleasePackRequires{Norca: tt.constraint}, tt.server)
			if tt.err == "" {
				must.NoError(t, err)
				return
			}
			must.ErrorContains(t, err, tt.err)
		})
	}

	must.NoError(t, checkReleaseRequirementsAt(nil, "0.3.0"))
	must.ErrorContains(t, checkReleaseRequirementsAt(&structs.ReleasePackRequires{Features: []string{"teleport"}}, "0.3.0"),
		`the module requires the feature "teleport"`)
}

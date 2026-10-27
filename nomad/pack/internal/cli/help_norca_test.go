// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/shoenig/test/must"
)

// TestCLI_HelpNoEnterpriseLeftovers checks that the help of run and plan does
// not describe Nomad Enterprise behaviour Norca does not have.
func TestCLI_HelpNoEnterpriseLeftovers(t *testing.T) {
	t.Parallel()
	for _, cmd := range []string{"run", "plan"} {
		t.Run(cmd, func(t *testing.T) {
			t.Parallel()
			r := runPackCmd(t, []string{cmd, "--help"})
			out := strings.Join(strings.Fields(r.cmdOut.String()), " ")
			must.StrContains(t, out, "--policy-override")
			must.StrContains(t, out, "Has no effect: Norca has no Sentinel policies")
			must.StrNotContains(t, out, "soft mandatory")
			must.StrContains(t, out, "If no ref is specified")
			must.StrNotContains(t, out, "defaults to latest.")
		})
	}

	r := runPackCmd(t, []string{"run", "--help"})
	out := strings.Join(strings.Fields(r.cmdOut.String()), " ")
	must.StrContains(t, out, "Not supported: a job that sets a Consul namespace is rejected")
}

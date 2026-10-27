// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
)

// TestHelp_MatchesCode checks help texts that used to describe flags or
// behaviour the commands do not have.
func TestHelp_MatchesCode(t *testing.T) {
	ci.Parallel(t)

	// The auth method of a binding rule cannot be changed, so update has no
	// -auth-method flag.
	update := (&ACLBindingRuleUpdateCommand{}).Help()
	must.StrNotContains(t, update, "-auth-method")
	must.StrContains(t, update, "cannot be changed")
	ui := cli.NewMockUi()
	code := (&ACLBindingRuleUpdateCommand{Meta: Meta{Ui: ui}}).Run([]string{"-auth-method=x", "id"})
	must.One(t, code)
	must.StrContains(t, ui.ErrorWriter.String(), "flag provided but not defined: -auth-method")

	// Quotas are not filtered by namespace access.
	for _, help := range []string{
		(&QuotaListCommand{}).Help(),
		(&QuotaStatusCommand{}).Help(),
		(&QuotaInspectCommand{}).Help(),
	} {
		must.StrNotContains(t, help, "filtered")
		must.StrNotContains(t, help, "access to any namespaces")
	}

	// quota status -json outputs the specification only.
	status := (&QuotaStatusCommand{}).Help()
	must.StrContains(t, status, "Output the quota specification in a JSON format")
	must.StrContains(t, status, "norca quota inspect -json")
}

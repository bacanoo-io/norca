// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"testing"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/command/agent"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
)

func TestACLPolicyDeleteCommand(t *testing.T) {
	ci.Parallel(t)
	config := func(c *agent.Config) {
		c.ACL.Enabled = true
	}

	srv, _, url := testServer(t, true, config)
	defer srv.Shutdown()

	state := srv.Agent.Server().State()

	// Bootstrap an initial ACL token
	token := srv.RootToken
	must.NotNil(t, token)

	// Create a test ACLPolicy
	policy := &structs.ACLPolicy{
		Name:  "testPolicy",
		Rules: acl.PolicyWrite,
	}
	policy.SetHash()
	must.NoError(t, state.UpsertACLPolicies(structs.MsgTypeTestSetup, 1000, []*structs.ACLPolicy{policy}))

	ui := cli.NewMockUi()
	cmd := &ACLPolicyDeleteCommand{Meta: Meta{Ui: ui, flagAddress: url}}

	// Delete the policy without a valid token fails
	invalidToken := mock.ACLToken()
	code := cmd.Run([]string{"-address=" + url, "-token=" + invalidToken.SecretID, policy.Name})
	must.One(t, code)

	// Delete the policy with a valid management token
	code = cmd.Run([]string{"-address=" + url, "-token=" + token.SecretID, policy.Name})
	must.Zero(t, code)

	// Check the output
	out := ui.OutputWriter.String()
	must.StrContains(t, out, fmt.Sprintf("Successfully deleted %s policy", policy.Name))
}

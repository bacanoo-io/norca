// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import "testing"

// An scp-like address takes the keys of the agent, never the token of https.
func TestGitCredentials_SSHAddress(t *testing.T) {
	t.Setenv("RIGGER_GIT_PASSWORD", "fixture")

	c, err := gitCredentials(t.Context(), "git@example.com:org/repo")
	if err != nil {
		t.Fatal(err)
	}

	if c == nil || !c.SSHAgent || c.Password != "" {
		t.Error("an scp-like address doesn't take the ssh agent alone")
	}
}

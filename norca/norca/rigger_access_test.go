// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The credentials of a registry go with a use of its repository that was
// allowed, not with its URL: a fetch that resolved no registry gets none.
func TestRiggerRegistry_CredentialsOfAUse(t *testing.T) {
	ci.Parallel(t)

	r := newRegistryTest(t)
	waitForActiveRootKey(t, r.s)

	const repo = "https://example.com/private"

	var written structs.VariablesApplyResponse

	must.NoError(t, r.s.RPC("Variables.Apply", &structs.VariablesApplyRequest{
		Op: structs.VarOpSet,
		Var: &structs.VariableDecrypted{
			VariableMetadata: structs.VariableMetadata{Namespace: structs.DefaultNamespace, Path: "creds/private"},
			Items:            structs.VariableItems{"password": "fixture"},
		},
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: r.token},
	}, &written))
	must.NoError(t, r.apply("private", map[string]any{
		"type": "git", "source": repo, "authSecret": map[string]any{"path": "creds/private"},
	}))

	regs, err := r.s.riggerRegistriesOf()
	must.NoError(t, err)

	got, err := regs.credentials(t.Context(), repo)
	must.NoError(t, err)
	must.Nil(t, got, must.Sprint("a URL alone takes the credentials"))

	ctx, err := r.s.useRiggerRepository(t.Context(), nil, repo)
	must.NoError(t, err)

	got, err = regs.credentials(ctx, repo)
	must.NoError(t, err)
	must.NotNil(t, got)
	must.Eq(t, "fixture", got.Password)

	other, err := regs.credentials(ctx, "https://example.com/other")
	must.NoError(t, err)
	must.Nil(t, other, must.Sprint("the use of one repository gives credentials to another"))
}

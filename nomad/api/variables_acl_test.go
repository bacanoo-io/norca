// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/hashicorp/nomad/api/internal/testutil"
	"github.com/shoenig/test/must"
)

// TestVariables_ReadPermissionDenied asserts that reading a variable without
// permission is reported as ErrVariablePermissionDenied, not as a missing
// variable, while Peek keeps returning nil.
func TestVariables_ReadPermissionDenied(t *testing.T) {
	testutil.Parallel(t)

	c, s, _ := makeACLClient(t, nil, nil)
	defer s.Stop()

	v := NewVariable("app/secret")
	v.Items["k"] = "v"
	_, _, err := c.Variables().Create(v, nil)
	must.NoError(t, err)

	token := createTestToken(t, c, "other-path", `namespace "default" {
  variables {
    path "other/*" { capabilities = ["read", "list"] }
  }
}`)
	q := &QueryOptions{AuthToken: token}

	assertDenied := func(t *testing.T, err error) {
		t.Helper()
		must.ErrorIs(t, err, ErrVariablePermissionDenied)
		must.False(t, errors.Is(err, ErrVariablePathNotFound))
		var respErr UnexpectedResponseError
		must.True(t, errors.As(err, &respErr))
		must.Eq(t, http.StatusForbidden, respErr.StatusCode())
	}

	t.Run("read", func(t *testing.T) {
		got, _, err := c.Variables().Read("app/secret", q)
		must.Nil(t, got)
		assertDenied(t, err)
	})

	t.Run("items", func(t *testing.T) {
		got, _, err := c.Variables().GetVariableItems("app/secret", q)
		must.Nil(t, got)
		assertDenied(t, err)
	})

	t.Run("peek", func(t *testing.T) {
		got, _, err := c.Variables().Peek("app/secret", q)
		must.NoError(t, err)
		must.Nil(t, got)
	})

	t.Run("not found", func(t *testing.T) {
		got, _, err := c.Variables().Read("other/missing", q)
		must.Nil(t, got)
		must.ErrorIs(t, err, ErrVariablePathNotFound)
	})
}

// createTestToken creates an ACL policy with the rules and a client token
// with the policy, and returns the token's secret ID.
func createTestToken(t *testing.T, c *Client, name, rules string) string {
	t.Helper()
	_, err := c.ACLPolicies().Upsert(&ACLPolicy{Name: name, Rules: rules}, nil)
	must.NoError(t, err)
	token, _, err := c.ACLTokens().Create(&ACLToken{Name: name, Type: "client", Policies: []string{name}}, nil)
	must.NoError(t, err)
	return token.SecretID
}

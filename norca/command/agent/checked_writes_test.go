// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/shoenig/test/must"
)

// The write endpoints of a namespace, a quota, an ACL policy and a network
// policy take ?cas=<modify index>, through the checked writes of the api
// client: 0 creates only what does not exist, an index changes only what
// still has it, and a refused write is a 409 wrapping api.ErrIndexConflict
// (review findings ATM-1, ATM-8).
func TestHTTP_CheckedWrites(t *testing.T) {
	ci.Parallel(t)
	httpACLTest(t, nil, func(s *TestAgent) {
		client := s.Client()
		client.SetSecretID(s.RootToken.SecretID)

		// conflict asserts a refused checked write.
		conflict := func(t *testing.T, err error) {
			t.Helper()
			must.True(t, errors.Is(err, api.ErrIndexConflict), must.Sprintf("not a conflict: %v", err))
			must.ErrorContains(t, err, "409")
		}

		// checked drives one resource: create, refused create, refused
		// stale update, update with the index read.
		checked := func(t *testing.T, write func(desc string, index uint64) error, read func() (string, uint64)) {
			t.Helper()
			must.NoError(t, write("one", 0))
			desc, index := read()
			must.Eq(t, "one", desc)

			err := write("two", 0)
			conflict(t, err)
			must.ErrorContains(t, err, "exists")
			err = write("two", index+100)
			conflict(t, err)
			desc, _ = read()
			must.Eq(t, "one", desc)

			must.NoError(t, write("two", index))
			desc, _ = read()
			must.Eq(t, "two", desc)
		}

		t.Run("namespace", func(t *testing.T) {
			checked(t, func(desc string, index uint64) error {
				_, err := client.Namespaces().CheckedRegister(&api.Namespace{Name: "cas-ns", Description: desc}, index, nil)
				return err
			}, func() (string, uint64) {
				ns, _, err := client.Namespaces().Info("cas-ns", nil)
				must.NoError(t, err)
				return ns.Description, ns.ModifyIndex
			})
		})

		t.Run("quota", func(t *testing.T) {
			checked(t, func(desc string, index uint64) error {
				_, err := client.Quotas().CheckedRegister(&api.QuotaSpec{Name: "cas-quota", Description: desc, Limits: []*api.QuotaLimit{{
					Region: "global", RegionLimit: &api.Resources{CPU: pointer.Of(1000)},
				}}}, index, nil)
				return err
			}, func() (string, uint64) {
				q, _, err := client.Quotas().Info("cas-quota", nil)
				must.NoError(t, err)
				return q.Description, q.ModifyIndex
			})
		})

		t.Run("ACL policy", func(t *testing.T) {
			checked(t, func(desc string, index uint64) error {
				_, err := client.ACLPolicies().CheckedUpsert(&api.ACLPolicy{Name: "cas-policy", Description: desc,
					Rules: `node { policy = "read" }`}, index, nil)
				return err
			}, func() (string, uint64) {
				p, _, err := client.ACLPolicies().Info("cas-policy", nil)
				must.NoError(t, err)
				return p.Description, p.ModifyIndex
			})
		})

		t.Run("network policy", func(t *testing.T) {
			// Without a namespace of its own: the policy and its check
			// are in the namespace of the request.
			must.NoError(t, func() error {
				_, err := client.Namespaces().Register(&api.Namespace{Name: "cas-np"}, nil)
				return err
			}())
			checked(t, func(desc string, index uint64) error {
				_, err := client.NetworkPolicies().CheckedRegister(&api.NetworkPolicy{Name: "cas-policy", Description: desc,
					DefaultDeny: []string{"egress"}}, index, &api.WriteOptions{Namespace: "cas-np"})
				return err
			}, func() (string, uint64) {
				p, _, err := client.NetworkPolicies().Info("cas-policy", &api.QueryOptions{Namespace: "cas-np"})
				must.NoError(t, err)
				return p.Description, p.ModifyIndex
			})
		})

		t.Run("bad cas", func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPut, "/v1/namespace/cas-ns?cas=one",
				strings.NewReader(`{"Name": "cas-ns"}`))
			must.NoError(t, err)
			setToken(req, s.RootToken)
			_, err = s.Server.NamespaceSpecificRequest(httptest.NewRecorder(), req)
			must.ErrorContains(t, err, "can not parse cas")
		})
	})
}

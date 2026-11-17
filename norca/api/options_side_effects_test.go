// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"context"
	"testing"

	"github.com/bacanoo-io/norca/norca/api/internal/testutil"
	"github.com/shoenig/test/must"
)

// Register sends the namespace of the policy without writing it into the
// write options of the caller.
func TestNetworkPolicies_Register_KeepsWriteOptions(t *testing.T) {
	testutil.Parallel(t)
	c, requests := recordingServer(t, `{}`)

	w := &WriteOptions{}
	_, err := c.NetworkPolicies().Register(&NetworkPolicy{Name: "a", Namespace: "prod"}, w)
	must.NoError(t, err)
	must.Eq(t, "", w.Namespace)
	_, err = c.NetworkPolicies().Delete("b", w)
	must.NoError(t, err)
	must.Eq(t, []string{
		"PUT /v1/network-policy/a?namespace=prod",
		"DELETE /v1/network-policy/b",
	}, requests())

	// The namespace of the options wins, and nil options work.
	_, err = c.NetworkPolicies().Register(&NetworkPolicy{Name: "a", Namespace: "prod"}, &WriteOptions{Namespace: "dev"})
	must.NoError(t, err)
	_, err = c.NetworkPolicies().Register(&NetworkPolicy{Name: "a", Namespace: "prod"}, nil)
	must.NoError(t, err)
	must.Eq(t, []string{
		"PUT /v1/network-policy/a?namespace=dev",
		"PUT /v1/network-policy/a?namespace=prod",
	}, requests())
}

// The parameters the event stream adds do not stay in the Params map of the
// caller: a Stream after a StreamExact with the same options is not exact.
func TestEventStream_KeepsParams(t *testing.T) {
	testutil.Parallel(t)
	c, requests := recordingServer(t, `{}`)

	q := &QueryOptions{Params: map[string]string{"foo": "bar"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := c.EventStream().StreamExact(ctx, nil, 5, q)
	must.NoError(t, err)
	must.Eq(t, map[string]string{"foo": "bar"}, q.Params)
	_, err = c.EventStream().Stream(ctx, nil, 0, q)
	must.NoError(t, err)
	must.Eq(t, map[string]string{"foo": "bar"}, q.Params)
	must.Eq(t, []string{
		"GET /v1/event/stream?exact=true&foo=bar&index=5",
		"GET /v1/event/stream?foo=bar&index=0",
	}, requests())
}

// PrefixList does not leave its prefix in the query options of the caller.
func TestAdmissionPolicies_PrefixList_KeepsQueryOptions(t *testing.T) {
	testutil.Parallel(t)
	c, requests := recordingServer(t, `[]`)

	q := &QueryOptions{}
	_, _, err := c.AdmissionPolicies().PrefixList("foo", q)
	must.NoError(t, err)
	must.Eq(t, "", q.Prefix)
	_, _, err = c.AdmissionPolicies().List(q)
	must.NoError(t, err)
	_, _, err = c.AdmissionPolicies().PrefixList("foo", nil)
	must.NoError(t, err)
	must.Eq(t, []string{
		"GET /v1/admission-policies?prefix=foo",
		"GET /v1/admission-policies",
		"GET /v1/admission-policies?prefix=foo",
	}, requests())
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

func TestServiceIntentionEndpoint_CRUD(t *testing.T) {
	ci.Parallel(t)

	srv, cleanup := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
	})
	defer cleanup()
	testutil.WaitForLeader(t, srv.RPC)

	write := structs.WriteRequest{Region: "global", Namespace: "default"}
	var resp structs.GenericResponse
	must.NoError(t, srv.RPC("ServiceIntention.Upsert", &structs.ServiceIntentionUpsertRequest{
		Intentions: []*structs.ServiceIntention{
			{Source: "web", Destination: "api", Action: "allow"},
			{SourceNamespace: "*", Source: "*", Destination: "api", Action: "deny"},
		},
		WriteRequest: write,
	}, &resp))
	must.Positive(t, resp.Index)

	var list structs.ServiceIntentionListResponse
	must.NoError(t, srv.RPC("ServiceIntention.List", &structs.ServiceIntentionListRequest{
		QueryOptions: structs.QueryOptions{Region: "global", Namespace: "default"}}, &list))
	must.Len(t, 2, list.Intentions)

	var get structs.SingleServiceIntentionResponse
	must.NoError(t, srv.RPC("ServiceIntention.Get", &structs.ServiceIntentionGetRequest{
		Source: "web", Destination: "api",
		QueryOptions: structs.QueryOptions{Region: "global", Namespace: "default"}}, &get))
	must.NotNil(t, get.Intention)
	must.Eq(t, "default", get.Intention.SourceNamespace)
	must.Eq(t, "allow", get.Intention.Action)

	// A blocking list returns after a change.
	done := make(chan *structs.ServiceIntentionListResponse, 1)
	go func() {
		var blocked structs.ServiceIntentionListResponse
		err := srv.RPC("ServiceIntention.List", &structs.ServiceIntentionListRequest{
			QueryOptions: structs.QueryOptions{Region: "global", Namespace: "default",
				MinQueryIndex: list.Index, MaxQueryTime: 10 * time.Second}}, &blocked)
		if err == nil {
			done <- &blocked
		}
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)

	// Invalid intentions, duplicates and unknown namespaces are rejected.
	err := srv.RPC("ServiceIntention.Upsert", &structs.ServiceIntentionUpsertRequest{
		Intentions: []*structs.ServiceIntention{{Source: "web", Destination: "api", Action: "maybe"}}, WriteRequest: write}, &resp)
	must.ErrorContains(t, err, "action must be")
	err = srv.RPC("ServiceIntention.Upsert", &structs.ServiceIntentionUpsertRequest{
		Intentions: []*structs.ServiceIntention{
			{Source: "a", Destination: "b", Action: "allow"}, {Source: "a", Destination: "b", Action: "deny"},
		}, WriteRequest: write}, &resp)
	must.ErrorContains(t, err, "duplicate")
	err = srv.RPC("ServiceIntention.Upsert", &structs.ServiceIntentionUpsertRequest{
		Intentions: []*structs.ServiceIntention{{Namespace: "nope", Source: "a", Destination: "b", Action: "allow"}}, WriteRequest: write}, &resp)
	must.ErrorContains(t, err, "does not exist")

	must.NoError(t, srv.RPC("ServiceIntention.Delete", &structs.ServiceIntentionDeleteRequest{
		Intentions: []*structs.ServiceIntention{{Source: "web", Destination: "api"}}, WriteRequest: write}, &resp))
	blocked, ok := <-done
	must.True(t, ok)
	must.Len(t, 1, blocked.Intentions)

	err = srv.RPC("ServiceIntention.Delete", &structs.ServiceIntentionDeleteRequest{
		Intentions: []*structs.ServiceIntention{{Source: "web", Destination: "api"}}, WriteRequest: write}, &resp)
	must.ErrorContains(t, err, "not found")
}

func TestServiceIntentionEndpoint_ACL(t *testing.T) {
	ci.Parallel(t)

	srv, root, cleanup := TestACLServer(t, func(c *Config) {
		c.NumSchedulers = 0
	})
	defer cleanup()
	testutil.WaitForLeader(t, srv.RPC)
	store := srv.fsm.State()

	ns := mock.Namespace()
	ns.Name = "prod"
	must.NoError(t, store.UpsertNamespaces(1000, []*structs.Namespace{ns}))

	writer := mock.CreatePolicyAndToken(t, store, 1001, "intention-writer",
		mock.NamespacePolicy("prod", "", []string{acl.NamespaceCapabilityWriteIntention, acl.NamespaceCapabilityReadJob}))
	jobWriter := mock.CreatePolicyAndToken(t, store, 1002, "job-writer",
		mock.NamespacePolicy("prod", acl.PolicyWrite, nil))
	reader := mock.CreatePolicyAndToken(t, store, 1003, "reader",
		mock.NamespacePolicy("prod", acl.PolicyRead, nil))

	upsert := func(token string) error {
		var resp structs.GenericResponse
		return srv.RPC("ServiceIntention.Upsert", &structs.ServiceIntentionUpsertRequest{
			Intentions:   []*structs.ServiceIntention{{Source: "web", Destination: "api", Action: "allow"}},
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: "prod", AuthToken: token},
		}, &resp)
	}
	list := func(namespace, token string) (*structs.ServiceIntentionListResponse, error) {
		var resp structs.ServiceIntentionListResponse
		err := srv.RPC("ServiceIntention.List", &structs.ServiceIntentionListRequest{
			QueryOptions: structs.QueryOptions{Region: "global", Namespace: namespace, AuthToken: token}}, &resp)
		return &resp, err
	}

	// write-intention is not part of the write policy shorthand.
	must.EqError(t, upsert(jobWriter.SecretID), structs.ErrPermissionDenied.Error())
	must.EqError(t, upsert(reader.SecretID), structs.ErrPermissionDenied.Error())
	node := mock.Node()
	must.NoError(t, store.UpsertNode(structs.MsgTypeTestSetup, 999, node))
	must.EqError(t, upsert(node.SecretID), structs.ErrPermissionDenied.Error())
	must.NoError(t, upsert(writer.SecretID))
	must.NoError(t, upsert(root.SecretID))

	// Reading needs read-job.
	out, err := list("prod", reader.SecretID)
	must.NoError(t, err)
	must.Len(t, 1, out.Intentions)
	_, err = list("default", reader.SecretID)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())
	out, err = list("*", reader.SecretID)
	must.NoError(t, err)
	must.Len(t, 1, out.Intentions)
	_, err = list("prod", "")
	must.EqError(t, err, structs.ErrPermissionDenied.Error())

	// A workload identity reads the intentions of its own namespace only.
	alloc := mock.Alloc()
	alloc.Namespace = "prod"
	alloc.Job.Namespace = "prod"
	must.NoError(t, store.UpsertJob(structs.MsgTypeTestSetup, 1004, nil, alloc.Job))
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, 1005, []*structs.Allocation{alloc}))
	token, _, err := srv.encrypter.SignClaims(alloc.ToTaskIdentityClaims(alloc.Job, "web"))
	must.NoError(t, err)
	out, err = list("prod", token)
	must.NoError(t, err)
	must.Len(t, 1, out.Intentions)
	_, err = list("default", token)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())
}

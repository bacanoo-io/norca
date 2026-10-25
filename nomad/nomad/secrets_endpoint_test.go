// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/backup"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/stream"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/secretstore"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

// secretCanary is the value the redaction tests search for everywhere a
// value must never appear.
const secretCanary = "canary-7f3a9c-s3cr3t-value"

// syncBuffer is a goroutine safe buffer for captured logs.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// secretTestServer starts a server with an unsealed secret store core.
func secretTestServer(t *testing.T, acl bool, cb func(*Config)) (*Server, *structs.ACLToken, func()) {
	t.Helper()
	var s *Server
	var root *structs.ACLToken
	var cleanup func()
	if acl {
		s, root, cleanup = TestACLServer(t, cb)
	} else {
		s, cleanup = TestServer(t, cb)
	}
	testutil.WaitForLeader(t, s.RPC)
	stop := TestSecretCore(t, s)
	return s, root, func() {
		stop()
		cleanup()
	}
}

func secretWriteReq(path, token string, items structs.SecretItems) *structs.SecretUpsertRequest {
	return &structs.SecretUpsertRequest{
		Path:  path,
		Items: items,
		WriteRequest: structs.WriteRequest{
			Region:    "global",
			Namespace: structs.DefaultNamespace,
			AuthToken: token,
		},
	}
}

func secretVersionsReq(path, token string, versions ...uint64) *structs.SecretVersionsRequest {
	return &structs.SecretVersionsRequest{
		Path:     path,
		Versions: versions,
		WriteRequest: structs.WriteRequest{
			Region:    "global",
			Namespace: structs.DefaultNamespace,
			AuthToken: token,
		},
	}
}

func secretQuery(token string) structs.QueryOptions {
	return structs.QueryOptions{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: token}
}

func TestSecretEndpoint_CRUD(t *testing.T) {
	ci.Parallel(t)
	s, _, cleanup := secretTestServer(t, false, nil)
	defer cleanup()
	codec := rpcClient(t, s)

	// Create, then a second version.
	var w structs.SecretWriteResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.SecretUpsertRPCMethod,
		secretWriteReq("app/db", "", structs.SecretItems{"password": "one"}), &w))
	must.Eq(t, 1, w.Secret.CurrentVersion)
	must.Eq(t, []string{"password"}, w.Secret.ItemKeys)
	must.StrHasPrefix(t, structs.SecretValueHashPrefix, w.Secret.ValueHash)
	must.Eq(t, structs.SecretDefaultMaxVersions, w.Secret.MaxVersions)
	hash1 := w.Secret.ValueHash

	req := secretWriteReq("app/db", "", structs.SecretItems{"password": "two", "user": "app"})
	req.Meta = &structs.SecretMeta{Description: "db", Owner: "team-a", Labels: map[string]string{"tier": "db"},
		RotateEvery: time.Hour, ExpiresAt: pointer.Of(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))}
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.SecretUpsertRPCMethod, req, &w))
	must.Eq(t, 2, w.Secret.CurrentVersion)
	must.Len(t, 2, w.Secret.Versions)
	must.NotEq(t, hash1, w.Secret.ValueHash)
	must.Eq(t, hash1, w.Secret.Version(1).ValueHash)
	must.Eq(t, "team-a", w.Secret.Owner)
	must.Eq(t, time.Hour, w.Secret.RotateEvery)
	modifyIndex := w.Secret.ModifyIndex

	// The same items give the same keyed hash.
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.SecretUpsertRPCMethod,
		secretWriteReq("app/db", "", structs.SecretItems{"password": "one"}), &w))
	must.Eq(t, hash1, w.Secret.ValueHash)
	must.Eq(t, "team-a", w.Secret.Owner, must.Sprint("metadata is kept without Meta"))

	// Values.
	var r structs.SecretReadResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.SecretReadRPCMethod,
		&structs.SecretReadRequest{Path: "app/db", QueryOptions: secretQuery("")}, &r))
	must.Eq(t, 3, r.Version)
	must.Eq(t, structs.SecretItems{"password": "one"}, r.Items)
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.SecretReadRPCMethod,
		&structs.SecretReadRequest{Path: "app/db", Version: 2, QueryOptions: secretQuery("")}, &r))
	must.Eq(t, structs.SecretItems{"password": "two", "user": "app"}, r.Items)

	// A missing secret is an empty response.
	r = structs.SecretReadResponse{}
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.SecretReadRPCMethod,
		&structs.SecretReadRequest{Path: "app/missing", QueryOptions: secretQuery("")}, &r))
	must.Nil(t, r.Secret)

	// Metadata and list.
	var m structs.SecretMetadataResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.SecretReadMetadataRPCMethod,
		&structs.SecretReadMetadataRequest{Path: "app/db", QueryOptions: secretQuery("")}, &m))
	must.Eq(t, 3, m.Secret.CurrentVersion)
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.SecretUpsertRPCMethod,
		secretWriteReq("app/cache", "", structs.SecretItems{"k": "v"}), &w))
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.SecretUpsertRPCMethod,
		secretWriteReq("other/x", "", structs.SecretItems{"k": "v"}), &w))
	var l structs.SecretListResponse
	q := secretQuery("")
	q.Prefix = "app/"
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.SecretListRPCMethod,
		&structs.SecretListRequest{QueryOptions: q}, &l))
	must.Len(t, 2, l.Secrets)
	must.Eq(t, "app/cache", l.Secrets[0].Path)
	must.Eq(t, "app/db", l.Secrets[1].Path)

	// Check-and-set.
	cas := req
	cas.Items = structs.SecretItems{"password": "three"}
	cas.Meta = nil
	cas.CheckIndex = pointer.Of(modifyIndex)
	err := msgpackrpc.CallWithCodec(codec, structs.SecretUpsertRPCMethod, cas, &w)
	must.ErrorContains(t, err, "409")
	cas.CheckIndex = pointer.Of(uint64(0))
	err = msgpackrpc.CallWithCodec(codec, structs.SecretUpsertRPCMethod, cas, &w)
	must.ErrorContains(t, err, "check index mismatch")
	create := secretWriteReq("app/new", "", structs.SecretItems{"k": "v"})
	create.CheckIndex = pointer.Of(uint64(0))
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.SecretUpsertRPCMethod, create, &w))

	// Metadata-only update writes no version.
	metaOnly := secretWriteReq("app/db", "", nil)
	metaOnly.Meta = &structs.SecretMeta{Owner: "team-b"}
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.SecretUpsertRPCMethod, metaOnly, &w))
	must.Eq(t, 3, w.Secret.CurrentVersion)
	must.Eq(t, "team-b", w.Secret.Owner)
	must.Eq(t, "", w.Secret.Description)
	err = msgpackrpc.CallWithCodec(codec, structs.SecretUpsertRPCMethod, secretWriteReq("app/none", "", nil), &w)
	must.ErrorContains(t, err, "secret not found")

	// Soft delete, undelete, destroy a version.
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.SecretDeleteRPCMethod, secretVersionsReq("app/db", ""), &w))
	must.True(t, w.Secret.Deleted)
	err = msgpackrpc.CallWithCodec(codec, structs.SecretReadRPCMethod,
		&structs.SecretReadRequest{Path: "app/db", QueryOptions: secretQuery("")}, &r)
	must.ErrorContains(t, err, "deleted")
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.SecretUndeleteRPCMethod, secretVersionsReq("app/db", ""), &w))
	must.False(t, w.Secret.Deleted)
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.SecretDestroyRPCMethod, secretVersionsReq("app/db", "", 2), &w))
	must.True(t, w.Secret.Version(2).Destroyed)
	must.Eq(t, "", w.Secret.Version(2).ValueHash)
	err = msgpackrpc.CallWithCodec(codec, structs.SecretReadRPCMethod,
		&structs.SecretReadRequest{Path: "app/db", Version: 2, QueryOptions: secretQuery("")}, &r)
	must.ErrorContains(t, err, "destroyed")
	err = msgpackrpc.CallWithCodec(codec, structs.SecretDeleteRPCMethod, secretVersionsReq("app/db", "", 42), &w)
	must.ErrorContains(t, err, "version 42 not found")

	// The destroyed version's blob is gone.
	blob, err := s.State().SecretStoreBlobByPath(nil, secretKVDataPrefix("default", "app/db")+"2")
	must.NoError(t, err)
	must.Nil(t, blob)

	// Destroying the whole secret removes the row and every blob.
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.SecretDestroyRPCMethod, secretVersionsReq("app/db", ""), &w))
	must.Nil(t, w.Secret)
	row, err := s.State().SecretByPath(nil, "default", "app/db")
	must.NoError(t, err)
	must.Nil(t, row)
	blobs, err := secretBlobs(s.State(), "default", "app/db")
	must.NoError(t, err)
	must.Len(t, 0, blobs)

	// Invalid input.
	err = msgpackrpc.CallWithCodec(codec, structs.SecretUpsertRPCMethod,
		secretWriteReq("bad//path", "", structs.SecretItems{"k": "v"}), &w)
	must.ErrorContains(t, err, "invalid secret path")
	err = msgpackrpc.CallWithCodec(codec, structs.SecretUpsertRPCMethod,
		secretWriteReq("big", "", structs.SecretItems{"k": strings.Repeat("x", structs.MaxSecretSize)}), &w)
	must.ErrorContains(t, err, "limited to")
}

func TestSecretEndpoint_MaxVersions(t *testing.T) {
	ci.Parallel(t)
	s, _, cleanup := secretTestServer(t, false, nil)
	defer cleanup()

	req := secretWriteReq("rotating", "", nil)
	req.Meta = &structs.SecretMeta{MaxVersions: 2}
	var w structs.SecretWriteResponse
	for i := 1; i <= 4; i++ {
		req.Items = structs.SecretItems{"v": fmt.Sprint(i)}
		must.NoError(t, s.RPC(structs.SecretUpsertRPCMethod, req, &w))
		req.Meta = nil
	}
	must.Eq(t, 4, w.Secret.CurrentVersion)
	must.Eq(t, 2, w.Secret.MaxVersions)
	must.Len(t, 2, w.Secret.Versions)
	must.Eq(t, 3, w.Secret.Versions[0].Version)

	// The aged out versions are gone from the barrier storage.
	for _, v := range []string{"1", "2"} {
		blob, err := s.State().SecretStoreBlobByPath(nil, secretKVDataPrefix("default", "rotating")+v)
		must.NoError(t, err)
		must.Nil(t, blob)
	}
	var r structs.SecretReadResponse
	err := s.RPC(structs.SecretReadRPCMethod,
		&structs.SecretReadRequest{Path: "rotating", Version: 1, QueryOptions: secretQuery("")}, &r)
	must.ErrorContains(t, err, "not found")
}

func TestSecretEndpoint_Sealed(t *testing.T) {
	ci.Parallel(t)
	s, cleanup := TestServer(t, nil)
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)

	var w structs.SecretWriteResponse
	err := s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("a", "", structs.SecretItems{"k": "v"}), &w)
	must.ErrorContains(t, err, "sealed")
	must.ErrorContains(t, err, "503")
	var r structs.SecretReadResponse
	must.NoError(t, s.RPC(structs.SecretReadRPCMethod,
		&structs.SecretReadRequest{Path: "a", QueryOptions: secretQuery("")}, &r))
	must.Nil(t, r.Secret)

	// Metadata reads need no core.
	var l structs.SecretListResponse
	must.NoError(t, s.RPC(structs.SecretListRPCMethod, &structs.SecretListRequest{QueryOptions: secretQuery("")}, &l))
	must.Len(t, 0, l.Secrets)

	// A sealed core rejects the reads of existing secrets.
	stop := TestSecretCore(t, s)
	must.NoError(t, s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("a", "", structs.SecretItems{"k": "v"}), &w))
	stop()
	err = s.RPC(structs.SecretReadRPCMethod, &structs.SecretReadRequest{Path: "a", QueryOptions: secretQuery("")}, &r)
	must.ErrorContains(t, err, "sealed")
	must.NoError(t, s.RPC(structs.SecretReadMetadataRPCMethod,
		&structs.SecretReadMetadataRequest{Path: "a", QueryOptions: secretQuery("")}, &structs.SecretMetadataResponse{}))
}

func TestSecretEndpoint_ACL(t *testing.T) {
	ci.Parallel(t)
	s, root, cleanup := secretTestServer(t, true, nil)
	defer cleanup()
	store := s.fsm.State()

	var w structs.SecretWriteResponse
	for _, p := range []string{"prod/db", "prod/api", "dev/db"} {
		must.NoError(t, s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq(p, root.SecretID, structs.SecretItems{"k": "v"}), &w))
	}

	tokens := map[string]*structs.ACLToken{
		// The read and write shorthands grant nothing on secrets.
		"shorthand": mock.CreatePolicyAndToken(t, store, 1001, "shorthand",
			`namespace "default" { policy = "write" }`),
		"list": mock.CreatePolicyAndToken(t, store, 1003, "list",
			`namespace "default" { capabilities = ["list-secrets"] }`),
		"read-prod": mock.CreatePolicyAndToken(t, store, 1005, "read-prod",
			`namespace "default" { secret "prod/*" { capabilities = ["read-secret"] } }`),
		"write-prod-deny-db": mock.CreatePolicyAndToken(t, store, 1007, "write-prod-deny-db",
			`namespace "default" {
			   capabilities = ["write-secret", "read-secret"]
			   secret "prod/db" { capabilities = ["deny"] }
			 }`),
		"destroy": mock.CreatePolicyAndToken(t, store, 1009, "destroy",
			`namespace "default" { capabilities = ["destroy-secret"] }`),
	}

	type check struct {
		token   string
		method  string
		path    string
		allowed bool
	}
	checks := []check{
		{"shorthand", structs.SecretReadRPCMethod, "prod/db", false},
		{"shorthand", structs.SecretUpsertRPCMethod, "prod/db", false},
		{"shorthand", structs.SecretReadMetadataRPCMethod, "prod/db", false},
		{"list", structs.SecretReadMetadataRPCMethod, "prod/db", true},
		{"list", structs.SecretReadRPCMethod, "prod/db", false},
		{"read-prod", structs.SecretReadRPCMethod, "prod/db", true},
		{"read-prod", structs.SecretReadMetadataRPCMethod, "prod/db", true},
		{"read-prod", structs.SecretReadRPCMethod, "dev/db", false},
		{"read-prod", structs.SecretUpsertRPCMethod, "prod/db", false},
		{"write-prod-deny-db", structs.SecretUpsertRPCMethod, "prod/api", true},
		{"write-prod-deny-db", structs.SecretReadRPCMethod, "prod/api", true},
		{"write-prod-deny-db", structs.SecretUpsertRPCMethod, "prod/db", false},
		{"write-prod-deny-db", structs.SecretReadRPCMethod, "prod/db", false},
		{"write-prod-deny-db", structs.SecretDeleteRPCMethod, "prod/api", true},
		{"write-prod-deny-db", structs.SecretUndeleteRPCMethod, "prod/api", true},
		{"write-prod-deny-db", structs.SecretDestroyRPCMethod, "dev/db", false},
		{"destroy", structs.SecretDestroyRPCMethod, "dev/db", true},
		{"destroy", structs.SecretUpsertRPCMethod, "dev/db", false},
	}
	for _, c := range checks {
		t.Run(fmt.Sprintf("%s %s %s", c.token, c.method, c.path), func(t *testing.T) {
			secret := tokens[c.token].SecretID
			var err error
			switch c.method {
			case structs.SecretReadRPCMethod:
				var r structs.SecretReadResponse
				err = s.RPC(c.method, &structs.SecretReadRequest{Path: c.path, QueryOptions: secretQuery(secret)}, &r)
			case structs.SecretReadMetadataRPCMethod:
				var r structs.SecretMetadataResponse
				err = s.RPC(c.method, &structs.SecretReadMetadataRequest{Path: c.path, QueryOptions: secretQuery(secret)}, &r)
			case structs.SecretUpsertRPCMethod:
				err = s.RPC(c.method, secretWriteReq(c.path, secret, structs.SecretItems{"k": "w"}), &w)
			default:
				err = s.RPC(c.method, secretVersionsReq(c.path, secret), &w)
			}
			if c.allowed {
				must.NoError(t, err)
			} else {
				must.EqError(t, err, structs.ErrPermissionDenied.Error())
			}
		})
	}

	// Lists are filtered per path.
	var l structs.SecretListResponse
	must.NoError(t, s.RPC(structs.SecretListRPCMethod,
		&structs.SecretListRequest{QueryOptions: secretQuery(tokens["read-prod"].SecretID)}, &l))
	paths := []string{}
	for _, stub := range l.Secrets {
		paths = append(paths, stub.Path)
	}
	must.Eq(t, []string{"prod/api", "prod/db"}, paths)
	err := s.RPC(structs.SecretListRPCMethod,
		&structs.SecretListRequest{QueryOptions: secretQuery(tokens["shorthand"].SecretID)}, &l)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())
}

func TestSecretEndpoint_WorkloadIdentity(t *testing.T) {
	ci.Parallel(t)
	s, root, cleanup := secretTestServer(t, true, nil)
	defer cleanup()
	store := s.fsm.State()

	alloc := mock.Alloc()
	alloc.ClientStatus = structs.AllocClientStatusRunning
	terminal := mock.Alloc()
	terminal.ClientStatus = structs.AllocClientStatusComplete
	terminal.Job = alloc.Job
	terminal.JobID = alloc.JobID
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, 1001, []*structs.Allocation{alloc, terminal}))
	token, _, err := s.encrypter.SignClaims(alloc.ToTaskIdentityClaims(alloc.Job, "web"))
	must.NoError(t, err)
	terminalToken, _, err := s.encrypter.SignClaims(terminal.ToTaskIdentityClaims(terminal.Job, "web"))
	must.NoError(t, err)

	jobPath := "nomad/jobs/" + alloc.JobID
	var w structs.SecretWriteResponse
	for _, p := range []string{jobPath, "jobs/" + alloc.JobID + "/db", "other"} {
		must.NoError(t, s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq(p, root.SecretID, structs.SecretItems{"k": "v"}), &w))
	}
	read := func(token, path string) error {
		var r structs.SecretReadResponse
		return s.RPC(structs.SecretReadRPCMethod, &structs.SecretReadRequest{Path: path, QueryOptions: secretQuery(token)}, &r)
	}

	// No implicit access, not even below the job's Variables path.
	must.EqError(t, read(token, jobPath), structs.ErrPermissionDenied.Error())

	// A policy bound to the job grants access.
	policy := &structs.ACLPolicy{
		Name:   "job-secrets",
		Rules:  fmt.Sprintf(`namespace "default" { secret "jobs/%s/*" { capabilities = ["read-secret"] } }`, alloc.JobID),
		JobACL: &structs.JobACL{Namespace: "default", JobID: alloc.JobID},
	}
	policy.SetHash()
	must.NoError(t, store.UpsertACLPolicies(structs.MsgTypeTestSetup, 1010, []*structs.ACLPolicy{policy}))
	must.NoError(t, read(token, "jobs/"+alloc.JobID+"/db"))
	must.EqError(t, read(token, "other"), structs.ErrPermissionDenied.Error())
	must.EqError(t, read(token, jobPath), structs.ErrPermissionDenied.Error())

	// The identity of a terminal allocation gets nothing.
	must.EqError(t, read(terminalToken, "jobs/"+alloc.JobID+"/db"), structs.ErrPermissionDenied.Error())
}

// TestSecretEndpoint_AuditAndRedaction checks the audit records on the
// SecretAccess topic and in the audit file, the Secret topic events, and
// that the value never appears in listings, metadata, events, the audit
// file, the server log or the backup browsing listing.
func TestSecretEndpoint_AuditAndRedaction(t *testing.T) {
	ci.Parallel(t)
	auditPath := filepath.Join(t.TempDir(), "secrets-audit.log")
	logs := &syncBuffer{}
	s, root, cleanup := secretTestServer(t, true, func(c *Config) {
		c.SecretsAudit = &secretstore.AuditFileConfig{Path: auditPath}
		c.Logger = hclog.NewInterceptLogger(&hclog.LoggerOptions{
			Name: "server", Level: hclog.Trace, Output: logs,
		})
	})
	defer cleanup()
	store := s.fsm.State()
	reader := mock.CreatePolicyAndToken(t, store, 1001, "list-only",
		`namespace "default" { capabilities = ["list-secrets"] }`)

	broker, err := store.EventBroker()
	must.NoError(t, err)
	sub, err := broker.Subscribe(&stream.SubscribeRequest{
		Topics:    map[structs.Topic][]string{structs.TopicSecret: {"*"}, structs.TopicSecretAccess: {"*"}},
		Namespace: "*",
	})
	must.NoError(t, err)
	defer sub.Unsubscribe()

	var w structs.SecretWriteResponse
	must.NoError(t, s.RPC(structs.SecretUpsertRPCMethod,
		secretWriteReq("app/db", root.SecretID, structs.SecretItems{"password": secretCanary}), &w))
	var r structs.SecretReadResponse
	must.NoError(t, s.RPC(structs.SecretReadRPCMethod,
		&structs.SecretReadRequest{Path: "app/db", QueryOptions: secretQuery(root.SecretID)}, &r))
	must.Eq(t, secretCanary, r.Items["password"])
	err = s.RPC(structs.SecretReadRPCMethod,
		&structs.SecretReadRequest{Path: "app/db", QueryOptions: secretQuery(reader.SecretID)}, &r)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())

	var outputs []any
	var l structs.SecretListResponse
	must.NoError(t, s.RPC(structs.SecretListRPCMethod, &structs.SecretListRequest{QueryOptions: secretQuery(reader.SecretID)}, &l))
	must.Len(t, 1, l.Secrets)
	outputs = append(outputs, l)
	var m structs.SecretMetadataResponse
	must.NoError(t, s.RPC(structs.SecretReadMetadataRPCMethod,
		&structs.SecretReadMetadataRequest{Path: "app/db", QueryOptions: secretQuery(reader.SecretID)}, &m))
	outputs = append(outputs, m, w)

	// Events: one Secret event, and the access records of the write, the
	// read and the denied read.
	var secretEvents, accessEvents []structs.Event
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for len(secretEvents) < 1 || len(accessEvents) < 3 {
		events, err := sub.Next(ctx)
		must.NoError(t, err)
		for _, e := range events.Events {
			switch e.Topic {
			case structs.TopicSecret:
				secretEvents = append(secretEvents, e)
			case structs.TopicSecretAccess:
				accessEvents = append(accessEvents, e)
			}
		}
	}
	must.Eq(t, structs.TypeSecretUpserted, secretEvents[0].Type)
	must.Eq(t, "app/db", secretEvents[0].Key)
	outputs = append(outputs, secretEvents, accessEvents)

	results := []string{}
	for _, e := range accessEvents {
		rec := e.Payload.(*structs.SecretAccessEvent)
		results = append(results, rec.Operation+":"+rec.Result)
		must.Eq(t, "global", rec.Region)
		must.Eq(t, "default", rec.Namespace)
		must.Eq(t, "app/db", rec.Path)
		if rec.Result == structs.SecretAccessResultOK {
			must.Eq(t, 1, rec.Version)
		}
		must.NotNil(t, rec.Accessor)
		must.NotEq(t, "", rec.Accessor.AccessorID)
	}
	must.Eq(t, []string{"write:ok", "read:ok", "read:denied"}, results)
	must.Eq(t, reader.AccessorID, accessEvents[2].Payload.(*structs.SecretAccessEvent).Accessor.AccessorID)

	// The audit file has a request and a response line for each allowed
	// access and a response line for the denied one.
	f, err := os.Open(auditPath)
	must.NoError(t, err)
	defer f.Close()
	var lines []map[string]any
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var line map[string]any
		must.NoError(t, json.Unmarshal(scanner.Bytes(), &line))
		lines = append(lines, line)
	}
	must.Len(t, 5, lines)
	must.Eq(t, "response", lines[4]["type"])
	must.Eq(t, "denied", lines[4]["result"])
	must.Eq(t, "app/db", lines[4]["path"])

	// Backup browsing of a snapshot of the state.
	fsm2 := testSnapshotRestore(t, s.fsm)
	contents, err := backupContents(&backup.Loaded{Record: &structs.BackupRecord{ID: "b1"}, State: fsm2.State()},
		structs.BackupContentSecrets, &backupContentsFilter{namespace: "*"})
	must.NoError(t, err)
	must.Len(t, 1, contents.Secrets)
	must.Eq(t, "app/db", contents.Secrets[0].Path)
	outputs = append(outputs, contents)

	for i, out := range outputs {
		data, err := json.Marshal(out)
		must.NoError(t, err)
		must.StrNotContains(t, string(data), secretCanary, must.Sprintf("output %d", i))
	}
	audit, err := os.ReadFile(auditPath)
	must.NoError(t, err)
	must.StrNotContains(t, string(audit), secretCanary)
	must.Positive(t, len(logs.String()))
	must.StrNotContains(t, logs.String(), secretCanary)
	assertSecretStoreCiphertext(t, s, secretCanary)
}

// TestSecretEndpoint_Forwarding writes and reads through followers: writes
// and value reads run on the leader's core, stale metadata reads are served
// by the follower.
func TestSecretEndpoint_Forwarding(t *testing.T) {
	ci.Parallel(t)
	var servers []*Server
	for i := 0; i < 3; i++ {
		s, cleanup := TestServer(t, func(c *Config) { c.BootstrapExpect = 3 })
		defer cleanup()
		servers = append(servers, s)
	}
	TestJoin(t, servers...)
	for _, s := range servers {
		testutil.WaitForLeader(t, s.RPC)
	}
	var leader, follower *Server
	waitForSecretStore(t, "leader", func() bool {
		for _, s := range servers {
			if s.IsLeader() {
				leader = s
			}
		}
		return leader != nil
	})
	for _, s := range servers {
		if s != leader {
			follower = s
		}
	}
	stop := TestSecretCore(t, leader)
	defer stop()

	var w structs.SecretWriteResponse
	must.NoError(t, follower.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("fwd", "", structs.SecretItems{"k": "v"}), &w))
	var r structs.SecretReadResponse
	q := secretQuery("")
	q.AllowStale = true // ignored for values: they are always read on the leader
	must.NoError(t, follower.RPC(structs.SecretReadRPCMethod, &structs.SecretReadRequest{Path: "fwd", QueryOptions: q}, &r))
	must.Eq(t, "v", r.Items["k"])

	waitForSecretStore(t, "replicated secret", func() bool {
		row, err := follower.State().SecretByPath(nil, "default", "fwd")
		return err == nil && row != nil
	})
	var m structs.SecretMetadataResponse
	must.NoError(t, follower.RPC(structs.SecretReadMetadataRPCMethod,
		&structs.SecretReadMetadataRequest{Path: "fwd", QueryOptions: q}, &m))
	must.Eq(t, 1, m.Secret.CurrentVersion)
}

func TestSecretEndpoint_BlockingMetadata(t *testing.T) {
	ci.Parallel(t)
	s, _, cleanup := secretTestServer(t, false, nil)
	defer cleanup()

	var w structs.SecretWriteResponse
	must.NoError(t, s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("watch", "", structs.SecretItems{"k": "1"}), &w))
	index := w.Secret.ModifyIndex

	go func() {
		time.Sleep(200 * time.Millisecond)
		var w structs.SecretWriteResponse
		_ = s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("watch", "", structs.SecretItems{"k": "2"}), &w)
	}()
	q := secretQuery("")
	q.MinQueryIndex = index
	q.MaxQueryTime = 5 * time.Second
	var m structs.SecretMetadataResponse
	start := time.Now()
	must.NoError(t, s.RPC(structs.SecretReadMetadataRPCMethod, &structs.SecretReadMetadataRequest{Path: "watch", QueryOptions: q}, &m))
	must.Greater(t, 150*time.Millisecond, time.Since(start))
	must.Eq(t, 2, m.Secret.CurrentVersion)
	must.Greater(t, index, m.Index)
}

func TestSecretEndpoint_BackupRestore(t *testing.T) {
	ci.Parallel(t)
	s, _, cleanup := secretTestServer(t, false, nil)
	defer cleanup()

	var w structs.SecretWriteResponse
	must.NoError(t, s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("restored", "", structs.SecretItems{"k": "one"}), &w))
	must.NoError(t, s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("restored", "", structs.SecretItems{"k": "two"}), &w))
	must.NoError(t, s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("restored/child", "", structs.SecretItems{"c": "c"}), &w))
	snapshot := testSnapshotRestore(t, s.fsm).State()
	backupRow, err := snapshot.SecretByPath(nil, "default", "restored")
	must.NoError(t, err)

	// Change the secret after the backup.
	must.NoError(t, s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("restored", "", structs.SecretItems{"k": "three"}), &w))
	must.NoError(t, s.RPC(structs.SecretDestroyRPCMethod, secretVersionsReq("restored", "", 1), &w))

	r := &backupRestorer{
		b:      &Backup{srv: s},
		args:   &structs.BackupRestoreRequest{WriteRequest: structs.WriteRequest{Region: "global"}},
		loaded: &backup.Loaded{Record: &structs.BackupRecord{ID: "b1"}, State: snapshot},
	}
	item := &structs.BackupRestoreItem{Type: structs.BackupRestoreItemSecret, Namespace: "default", ID: "restored"}
	must.NoError(t, validateBackupRestoreItem(item))

	// Without a check index an existing secret is not overwritten.
	result := r.restore(item)
	must.StrContains(t, result.Error, "give that check index")
	item.CheckIndex = pointer.Of(w.Secret.ModifyIndex)
	result = r.restore(item)
	must.Eq(t, "", result.Error)
	must.Eq(t, "update", result.Action)

	var m structs.SecretMetadataResponse
	must.NoError(t, s.RPC(structs.SecretReadMetadataRPCMethod,
		&structs.SecretReadMetadataRequest{Path: "restored", QueryOptions: secretQuery("")}, &m))
	must.Eq(t, 2, m.Secret.CurrentVersion)
	must.Len(t, 2, m.Secret.Versions)
	must.False(t, m.Secret.Version(1).Destroyed)
	must.Eq(t, backupRow.ValueHash, m.Secret.ValueHash)
	for _, v := range []uint64{1, 2} {
		var rr structs.SecretReadResponse
		must.NoError(t, s.RPC(structs.SecretReadRPCMethod,
			&structs.SecretReadRequest{Path: "restored", Version: v, QueryOptions: secretQuery("")}, &rr))
		must.Eq(t, map[uint64]string{1: "one", 2: "two"}[v], rr.Items["k"])
	}
	// The secret below the restored path is untouched.
	var rr structs.SecretReadResponse
	must.NoError(t, s.RPC(structs.SecretReadRPCMethod,
		&structs.SecretReadRequest{Path: "restored/child", QueryOptions: secretQuery("")}, &rr))
	must.Eq(t, "c", rr.Items["c"])
}

func TestSecretFSM_SnapshotRestore(t *testing.T) {
	ci.Parallel(t)
	s, _, cleanup := secretTestServer(t, false, nil)
	defer cleanup()

	var w structs.SecretWriteResponse
	must.NoError(t, s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("snap", "", structs.SecretItems{"k": "v"}), &w))
	fsm2 := testSnapshotRestore(t, s.fsm)
	got, err := fsm2.State().SecretByPath(nil, "default", "snap")
	must.NoError(t, err)
	must.Eq(t, w.Secret, got)
	blobs, err := secretBlobs(fsm2.State(), "default", "snap")
	must.NoError(t, err)
	must.Len(t, 2, blobs)
}

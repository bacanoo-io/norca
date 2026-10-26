// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/stream"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

// secretDeliveryJob returns a job with a secret block and a secret_access
// binding of its group for jobs/${job}/*.
func secretDeliveryJob(id string) *structs.Job {
	job := mock.Job()
	job.ID, job.Name = id, id
	tg := job.TaskGroups[0]
	tg.SecretAccess = []*structs.SecretAccess{{Path: "jobs/${job}/*"}}
	tg.Tasks[0].Secrets = []*structs.TaskSecret{{
		Name: "db",
		Path: "jobs/${job}/db",
		File: &structs.TaskSecretFile{Destination: "secrets/db.env"},
	}}
	return job
}

// TestJobEndpoint_SecretAccess_NoEscalation checks that a job's
// secret_access bindings can't grant more than its submitter holds.
func TestJobEndpoint_SecretAccess_NoEscalation(t *testing.T) {
	ci.Parallel(t)
	s, root, cleanup := TestACLServer(t, func(c *Config) { c.NumSchedulers = 0 })
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)
	store := s.fsm.State()

	reader := mock.CreatePolicyAndToken(t, store, 1001, "web-reader", `namespace "default" {
  policy = "write"
  secret "jobs/web/*" { capabilities = ["read-secret"] }
}`)
	writer := mock.CreatePolicyAndToken(t, store, 1002, "writer", `namespace "default" {
  policy = "write"
  secret "jobs/*" { capabilities = ["write-secret"] }
}`)
	narrow := mock.CreatePolicyAndToken(t, store, 1003, "narrow", `namespace "default" {
  policy = "write"
  secret "jobs/*" { capabilities = ["read-secret"] }
  secret "jobs/web/admin" { capabilities = ["list-secrets"] }
}`)

	// The submitter holds read-secret on the binding's path.
	_, err := admissionRegisterJob(t, s, secretDeliveryJob("web"), reader.SecretID)
	must.NoError(t, err)

	// Not for another job's path.
	_, err = admissionRegisterJob(t, s, secretDeliveryJob("api"), reader.SecretID)
	must.ErrorContains(t, err, `secret_access "jobs/api/*" of group "web" requires read-secret`)

	// write-secret alone doesn't allow handing out read access.
	_, err = admissionRegisterJob(t, s, secretDeliveryJob("web"), writer.SecretID)
	must.ErrorContains(t, err, "Permission denied")

	// A narrower rule without read-secret below the pattern blocks it.
	_, err = admissionRegisterJob(t, s, secretDeliveryJob("web"), narrow.SecretID)
	must.ErrorContains(t, err, "Permission denied")
	job := secretDeliveryJob("web")
	job.TaskGroups[0].SecretAccess = []*structs.SecretAccess{{Path: "jobs/${job}/db"}}
	_, err = admissionRegisterJob(t, s, job, narrow.SecretID)
	must.NoError(t, err)

	// A task binding with ${task}.
	job = secretDeliveryJob("web")
	job.TaskGroups[0].SecretAccess = nil
	job.TaskGroups[0].Tasks[0].SecretAccess = []*structs.SecretAccess{{Path: "jobs/${job}/${task}"}}
	_, err = admissionRegisterJob(t, s, job, reader.SecretID)
	must.NoError(t, err)

	// Plan applies the same check.
	codec := rpcClient(t, s)
	plan := &structs.JobPlanRequest{
		Job:          secretDeliveryJob("api"),
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: reader.SecretID},
	}
	var planResp structs.JobPlanResponse
	err = msgpackrpc.CallWithCodec(codec, "Job.Plan", plan, &planResp)
	must.ErrorContains(t, err, "requires read-secret")

	// A management token may grant anything.
	_, err = admissionRegisterJob(t, s, secretDeliveryJob("api"), root.SecretID)
	must.NoError(t, err)
}

// TestSecretEndpoint_SecretAccessBinding checks the implicit grant of a
// job's secret_access binding to its workload identities, and that a denied
// read of a task without a grant is audited.
func TestSecretEndpoint_SecretAccessBinding(t *testing.T) {
	ci.Parallel(t)
	s, root, cleanup := secretTestServer(t, true, nil)
	defer cleanup()
	store := s.fsm.State()

	alloc := mock.Alloc()
	alloc.ClientStatus = structs.AllocClientStatusRunning
	alloc.Job.TaskGroups[0].Tasks[0].SecretAccess = []*structs.SecretAccess{{Path: "jobs/${job}/${task}/*"}}
	other := mock.Alloc()
	other.ClientStatus = structs.AllocClientStatusRunning
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, 1001, []*structs.Allocation{alloc, other}))
	token, _, err := s.encrypter.SignClaims(alloc.ToTaskIdentityClaims(alloc.Job, "web"))
	must.NoError(t, err)
	otherToken, _, err := s.encrypter.SignClaims(other.ToTaskIdentityClaims(other.Job, "web"))
	must.NoError(t, err)

	granted := "jobs/" + alloc.JobID + "/web/db"
	var w structs.SecretWriteResponse
	for _, p := range []string{granted, "jobs/" + alloc.JobID + "/db"} {
		must.NoError(t, s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq(p, root.SecretID, structs.SecretItems{"k": "v"}), &w))
	}

	broker, err := store.EventBroker()
	must.NoError(t, err)
	sub, err := broker.Subscribe(&stream.SubscribeRequest{
		Topics:    map[structs.Topic][]string{structs.TopicSecretAccess: {"*"}},
		Namespace: "*",
	})
	must.NoError(t, err)
	defer sub.Unsubscribe()

	read := func(token, path string) error {
		var r structs.SecretReadResponse
		return s.RPC(structs.SecretReadRPCMethod, &structs.SecretReadRequest{Path: path, QueryOptions: secretQuery(token)}, &r)
	}
	must.NoError(t, read(token, granted))
	var m structs.SecretMetadataResponse
	must.NoError(t, s.RPC(structs.SecretReadMetadataRPCMethod,
		&structs.SecretReadMetadataRequest{Path: granted, QueryOptions: secretQuery(token)}, &m))
	must.NotNil(t, m.Secret)

	// Outside the binding, and for a task of another job, nothing.
	must.EqError(t, read(token, "jobs/"+alloc.JobID+"/db"), structs.ErrPermissionDenied.Error())
	must.EqError(t, read(otherToken, granted), structs.ErrPermissionDenied.Error())

	// The denials are audited with the workload identity.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var recs []*structs.SecretAccessEvent
	for len(recs) < 3 {
		events, err := sub.Next(ctx)
		must.NoError(t, err)
		for _, e := range events.Events {
			if rec := e.Payload.(*structs.SecretAccessEvent); rec.Operation == structs.SecretOpRead {
				recs = append(recs, rec)
			}
		}
	}
	must.Eq(t, structs.SecretAccessResultOK, recs[0].Result)
	must.Eq(t, structs.SecretAccessResultDenied, recs[1].Result)
	must.Eq(t, alloc.JobID, recs[1].Accessor.JobID)
	must.Eq(t, alloc.ID, recs[1].Accessor.AllocationID)
	must.Eq(t, structs.SecretAccessResultDenied, recs[2].Result)
	must.Eq(t, other.JobID, recs[2].Accessor.JobID)

	// A terminal allocation's identity loses the grant.
	terminal := alloc.Copy()
	terminal.ClientStatus = structs.AllocClientStatusComplete
	terminal.DesiredStatus = structs.AllocDesiredStatusStop
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, 1100, []*structs.Allocation{terminal}))
	must.EqError(t, read(token, granted), structs.ErrPermissionDenied.Error())
}

// TestJobEndpoint_SecretsNamespaceDefault checks that the namespace's
// default provider is captured on the job at registration.
func TestJobEndpoint_SecretsNamespaceDefault(t *testing.T) {
	ci.Parallel(t)
	s, cleanup := admissionServer(t)
	defer cleanup()

	ns := mock.Namespace()
	ns.Name = "prod"
	ns.Meta = map[string]string{structs.NamespaceMetaSecretsDefaultProvider: "bao"}
	must.NoError(t, s.fsm.State().UpsertNamespaces(1000, []*structs.Namespace{ns}))

	job := secretDeliveryJob("web")
	job.Namespace = "prod"
	_, err := admissionRegisterJob(t, s, job, "")
	must.NoError(t, err)
	stored, err := s.State().JobByID(nil, "prod", "web")
	must.NoError(t, err)
	must.Eq(t, &structs.SecretsConfig{NamespaceDefaultProvider: "bao"}, stored.Secrets)
	must.Eq(t, "bao", stored.SecretDefaultProvider(stored.TaskGroups[0]))

	// A submitted namespace default is replaced; the job's own default
	// stays and wins.
	job = secretDeliveryJob("api")
	job.Secrets = &structs.SecretsConfig{DefaultProvider: "norca", NamespaceDefaultProvider: "spoofed"}
	_, err = admissionRegisterJob(t, s, job, "")
	must.NoError(t, err)
	stored, err = s.State().JobByID(nil, "default", "api")
	must.NoError(t, err)
	must.Eq(t, &structs.SecretsConfig{DefaultProvider: "norca"}, stored.Secrets)
}

// TestSecretEndpoint_RecordAccess checks the client audit records of
// fetches from external providers.
func TestSecretEndpoint_RecordAccess(t *testing.T) {
	ci.Parallel(t)
	s, root, cleanup := secretTestServer(t, true, nil)
	defer cleanup()
	store := s.fsm.State()

	alloc := mock.Alloc()
	alloc.ClientStatus = structs.AllocClientStatusRunning
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, 1001, []*structs.Allocation{alloc}))
	token, _, err := s.encrypter.SignClaims(alloc.ToTaskIdentityClaims(alloc.Job, "web"))
	must.NoError(t, err)

	broker, err := store.EventBroker()
	must.NoError(t, err)
	sub, err := broker.Subscribe(&stream.SubscribeRequest{
		Topics:    map[structs.Topic][]string{structs.TopicSecretAccess: {"*"}},
		Namespace: "*",
	})
	must.NoError(t, err)
	defer sub.Unsubscribe()

	record := func(token string, rec *structs.SecretAccessEvent) error {
		var reply structs.GenericResponse
		return s.RPC(structs.SecretRecordAccessRPCMethod, &structs.SecretRecordAccessRequest{
			Record:       rec,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: token},
		}, &reply)
	}
	must.NoError(t, record(token, &structs.SecretAccessEvent{
		Path: "kv/data/app", Provider: "vault", ProviderVersion: "3", Result: structs.SecretAccessResultOK,
		Accessor: &structs.SecretAccessor{JobID: "forged"},
	}))
	must.ErrorContains(t, record(token, &structs.SecretAccessEvent{Path: "app", Provider: "norca"}), "external providers")
	must.ErrorContains(t, record(root.SecretID, &structs.SecretAccessEvent{Path: "app", Provider: "vault"}), "workload identity")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events, err := sub.Next(ctx)
	must.NoError(t, err)
	rec := events.Events[0].Payload.(*structs.SecretAccessEvent)
	must.Eq(t, "vault", rec.Provider)
	must.Eq(t, "3", rec.ProviderVersion)
	must.Eq(t, "kv/data/app", rec.Path)
	must.Eq(t, structs.SecretOpRead, rec.Operation)
	must.Eq(t, structs.SecretAccessResultOK, rec.Result)
	must.Eq(t, alloc.JobID, rec.Accessor.JobID, must.Sprint("the accessor is the workload identity"))
}

// TestJobEndpoint_Admission_Secrets checks example admission policies on
// the secrets a job references (request.secrets).
func TestJobEndpoint_Admission_Secrets(t *testing.T) {
	ci.Parallel(t)
	s, cleanup := admissionServer(t)
	defer cleanup()

	ns := mock.Namespace()
	ns.Name = "prod"
	must.NoError(t, s.fsm.State().UpsertNamespaces(1000, []*structs.Namespace{ns}))

	upsertAdmissionPolicies(t, s,
		// prod must use the bao provider
		testAdmissionPolicy("prod-bao", []string{"prod"}, "deny",
			`request.secrets.all(s, s.effective_provider == "bao")`),
		// no secrets in environment variables in prod
		testAdmissionPolicy("prod-no-env", []string{"prod"}, "deny",
			`request.secrets.all(s, size(s.env) == 0)`),
		// a job may only reference its own secrets
		testAdmissionPolicy("own-paths", nil, "deny",
			`request.secrets.all(s, s.path.startsWith("jobs/" + job.ID + "/"))`),
	)

	// Fallback to the built-in store is rejected in prod.
	job := secretDeliveryJob("web")
	job.Namespace = "prod"
	_, err := admissionRegisterJob(t, s, job, "")
	must.ErrorContains(t, err, "prod-bao violated")

	job = secretDeliveryJob("web")
	job.Namespace = "prod"
	job.TaskGroups[0].Tasks[0].Secrets[0].Provider = "bao"
	_, err = admissionRegisterJob(t, s, job, "")
	must.NoError(t, err)

	job = secretDeliveryJob("api")
	job.Namespace = "prod"
	job.Secrets = &structs.SecretsConfig{DefaultProvider: "bao"}
	job.TaskGroups[0].Tasks[0].Secrets[0].Env = map[string]string{"DB_PASSWORD": "password"}
	_, err = admissionRegisterJob(t, s, job, "")
	must.ErrorContains(t, err, "prod-no-env violated")

	// Paths of other jobs, also through nomadSecret calls of templates.
	job = secretDeliveryJob("web")
	job.TaskGroups[0].Tasks[0].Secrets[0].Path = "jobs/billing/db"
	_, err = admissionRegisterJob(t, s, job, "")
	must.ErrorContains(t, err, "own-paths violated")
	job = secretDeliveryJob("web")
	job.TaskGroups[0].Tasks[0].Templates = []*structs.Template{{
		EmbeddedTmpl: `{{ with nomadSecret "bao:jobs/billing/db" }}{{ .password }}{{ end }}`,
		DestPath:     "local/x", ChangeMode: "noop",
	}}
	_, err = admissionRegisterJob(t, s, job, "")
	must.ErrorContains(t, err, "own-paths violated")

	_, err = admissionRegisterJob(t, s, secretDeliveryJob("web"), "")
	must.NoError(t, err)
}

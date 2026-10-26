// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/secrets"
	"github.com/hashicorp/nomad/client/vaultcompat"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/stream"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/shoenig/test/must"
)

// vaultCompatJob returns a job whose task has a vault block and a Vault
// secret template.
func vaultCompatJob(id string) *structs.Job {
	job := mock.Job()
	job.ID, job.Name = id, id
	task := job.TaskGroups[0].Tasks[0]
	task.Vault = &structs.Vault{Policies: []string{"app"}, ChangeMode: structs.VaultChangeModeRestart}
	task.Templates = []*structs.Template{{
		EmbeddedTmpl: `{{ with secret "kv/data/app/db?version=2" }}{{ .Data.data.password }}{{ end }}` +
			`{{ with nomadSecret "jobs/x" }}{{ end }}`,
		DestPath:   "local/app.conf",
		ChangeMode: structs.TemplateChangeModeRestart,
	}}
	return job
}

// TestJobEndpoint_VaultFallback checks that without Vault, jobs with vault
// blocks are accepted with a warning and constrained to the clients that
// serve vault blocks from the built-in store, and that the fallback can be
// turned off.
func TestJobEndpoint_VaultFallback(t *testing.T) {
	ci.Parallel(t)
	s, cleanup := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
		c.VaultConfig.Enabled = pointer.Of(false)
		c.SecretsVaultFallback = true
	})
	defer cleanup()

	resp, err := admissionRegisterJob(t, s, vaultCompatJob("web"), "")
	must.NoError(t, err)
	must.StrContains(t, resp.Warnings, "served by the built-in secret store")
	stored, err := s.State().JobByID(nil, "default", "web")
	must.NoError(t, err)
	must.SliceContains(t, stored.TaskGroups[0].Constraints, vaultCompatConstraint)
	for _, c := range stored.TaskGroups[0].Constraints {
		must.NotEq(t, attrVaultVersion, c.LTarget)
	}

	// Turned off: Nomad's behaviour.
	s.config.SecretsVaultFallback = false
	_, err = admissionRegisterJob(t, s, vaultCompatJob("api"), "")
	must.ErrorContains(t, err, "Vault not enabled")
}

// TestJobImpliedConstraints_VaultCompat checks the implied constraint of
// vault blocks with and without a Vault integration.
func TestJobImpliedConstraints_VaultCompat(t *testing.T) {
	ci.Parallel(t)
	srv := func(vault, fallback bool) *Server {
		return &Server{config: &Config{
			VaultConfig:          &config.VaultConfig{Enabled: pointer.Of(vault)},
			SecretsVaultFallback: fallback,
		}}
	}
	constraintOf := func(s *Server) *structs.Constraint {
		job, _, err := jobImpliedConstraints{srv: s}.Mutate(vaultCompatJob("web"))
		must.NoError(t, err)
		for _, c := range job.TaskGroups[0].Constraints {
			if c.LTarget == attrVaultVersion || c.LTarget == attrVaultCompat {
				return c
			}
		}
		return nil
	}
	must.Eq(t, vaultConstraint, constraintOf(srv(true, true)))
	must.Eq(t, vaultConstraint, constraintOf(srv(false, false)))
	must.Eq(t, vaultCompatConstraint, constraintOf(srv(false, true)))
	must.Eq(t, vaultConstraint, constraintOf(nil))
}

// TestAdmission_VaultSecretRefs checks the Vault-style lookups in the
// request.secrets list of admission policies.
func TestAdmission_VaultSecretRefs(t *testing.T) {
	ci.Parallel(t)
	job := vaultCompatJob("web")
	byKey := func(vaultEnabled bool) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, v := range admissionSecretRefsValue(job, vaultEnabled) {
			m := v.(map[string]any)
			out[m["source"].(string)+":"+m["path"].(string)] = m
		}
		return out
	}

	refs := byKey(false)
	must.MapContainsKeys(t, refs, []string{"vault:", "vault_template:kv/data/app/db", "template:jobs/x"})
	must.Eq[any](t, "vault", refs["vault:"]["name"])
	must.Eq[any](t, "", refs["vault:"]["effective_provider"])
	must.Eq[any](t, "", refs["vault_template:kv/data/app/db"]["effective_provider"])

	refs = byKey(true)
	must.Eq[any](t, "vault", refs["vault:"]["effective_provider"])
	must.Eq[any](t, "vault", refs["vault_template:kv/data/app/db"]["effective_provider"])
	must.Eq[any](t, "", refs["template:jobs/x"]["effective_provider"], must.Sprint("not Vault-style"))

	// A namespace default applies to Vault-style lookups too.
	job.Secrets = &structs.SecretsConfig{NamespaceDefaultProvider: "bao"}
	refs = byKey(true)
	must.Eq[any](t, "bao", refs["vault:"]["effective_provider"])
}

// TestVaultCompat_Endpoint_Server reads through the client's Vault-compatible
// endpoint from a real server: the task's secret_access grant decides, the
// value is read with the task's workload identity, and the server audits
// the read and the denial with that identity.
func TestVaultCompat_Endpoint_Server(t *testing.T) {
	ci.Parallel(t)
	s, root, cleanup := secretTestServer(t, true, nil)
	defer cleanup()
	store := s.fsm.State()

	alloc := mock.Alloc()
	alloc.ClientStatus = structs.AllocClientStatusRunning
	task := alloc.Job.TaskGroups[0].Tasks[0]
	task.Vault = &structs.Vault{Policies: []string{"app"}}
	task.SecretAccess = []*structs.SecretAccess{{Path: "app/*"}}
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, 1001, []*structs.Allocation{alloc}))
	wi, _, err := s.encrypter.SignClaims(alloc.ToTaskIdentityClaims(alloc.Job, task.Name))
	must.NoError(t, err)
	alloc.SignedIdentities = map[string]string{task.Name: wi}

	const canary = "server-canary-0d1e"
	var w structs.SecretWriteResponse
	must.NoError(t, s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("app/db", root.SecretID, structs.SecretItems{"password": canary}), &w))
	must.NoError(t, s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("other/db", root.SecretID, structs.SecretItems{"password": "x"}), &w))

	broker, err := store.EventBroker()
	must.NoError(t, err)
	sub, err := broker.Subscribe(&stream.SubscribeRequest{
		Topics:    map[structs.Topic][]string{structs.TopicSecretAccess: {"*"}},
		Namespace: "*",
	})
	must.NoError(t, err)
	defer sub.Unsubscribe()

	reg := secrets.NewRegistry(secrets.NewNorcaProvider(s, "global"), nil, nil)
	endpoint := vaultcompat.New(vaultcompat.Config{
		Reader:     &secrets.VaultReader{Registry: reg, Mapper: secrets.NewVaultMapper(nil)},
		NodeSecret: "node-secret",
	})
	defer endpoint.Shutdown()
	tokens, err := endpoint.Client().DeriveToken(alloc, []string{task.Name})
	must.NoError(t, err)

	conf := vaultapi.DefaultConfig()
	conf.Address = endpoint.VaultAddr(alloc)
	conf.MaxRetries = 0
	vc, err := vaultapi.NewClient(conf)
	must.NoError(t, err)
	vc.SetToken(tokens[task.Name])

	kv, err := vc.KVv2("secret").Get(context.Background(), "app/db")
	must.NoError(t, err)
	must.Eq(t, canary, kv.Data["password"])
	md, err := vc.KVv2("secret").GetMetadata(context.Background(), "app/db")
	must.NoError(t, err)
	must.Eq(t, 1, md.CurrentVersion)

	_, err = vc.KVv2("secret").Get(context.Background(), "other/db")
	var rerr *vaultapi.ResponseError
	must.True(t, errors.As(err, &rerr))
	must.Eq(t, http.StatusForbidden, rerr.StatusCode)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var recs []*structs.SecretAccessEvent
	for len(recs) < 2 {
		events, err := sub.Next(ctx)
		must.NoError(t, err)
		for _, e := range events.Events {
			if rec := e.Payload.(*structs.SecretAccessEvent); rec.Operation == structs.SecretOpRead {
				recs = append(recs, rec)
			}
		}
	}
	must.Eq(t, "app/db", recs[0].Path)
	must.Eq(t, structs.SecretAccessResultOK, recs[0].Result)
	must.Eq(t, "other/db", recs[1].Path)
	must.Eq(t, structs.SecretAccessResultDenied, recs[1].Result)
	must.Eq(t, alloc.ID, recs[1].Accessor.AllocationID)
	for _, r := range recs {
		must.False(t, strings.Contains(r.Error, canary))
	}
}

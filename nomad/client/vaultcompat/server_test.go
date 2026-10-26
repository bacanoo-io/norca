// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package vaultcompat

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	vaultapi "github.com/hashicorp/vault/api"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/secrets"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

const canary = "vaultcompat-canary-7f3a9"

// fakeServers serves Secret.Read and Secret.ReadMetadata like the servers:
// each workload identity token may read the paths below its grants, and
// every value read is recorded as the servers' audit would.
type fakeServers struct {
	lock    sync.Mutex
	secrets map[string]structs.SecretItems
	grants  map[string][]string // identity token -> path prefixes
	audit   []string            // "<token> <path> <result>"
}

func (f *fakeServers) allowed(token, path string) bool {
	for _, p := range f.grants[token] {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func (f *fakeServers) RPC(method string, args, reply any) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	switch method {
	case structs.SecretReadRPCMethod:
		a := args.(*structs.SecretReadRequest)
		if !f.allowed(a.AuthToken, a.Path) {
			f.audit = append(f.audit, a.AuthToken+" "+a.Path+" denied")
			return structs.ErrPermissionDenied
		}
		items, ok := f.secrets[a.Path]
		if !ok {
			f.audit = append(f.audit, a.AuthToken+" "+a.Path+" not-found")
			return errors.New("RPC Error:: 404,secret not found")
		}
		f.audit = append(f.audit, a.AuthToken+" "+a.Path+" ok")
		r := reply.(*structs.SecretReadResponse)
		r.Secret = &structs.Secret{Path: a.Path, CurrentVersion: 3, MaxVersions: 10,
			Versions: []*structs.SecretVersion{{Version: 2}, {Version: 3}}}
		r.Items = items.Copy()
		r.Version = 3
		if a.Version != 0 {
			r.Version = a.Version
		}
	case structs.SecretReadMetadataRPCMethod:
		a := args.(*structs.SecretReadMetadataRequest)
		if !f.allowed(a.AuthToken, a.Path) {
			return structs.ErrPermissionDenied
		}
		r := reply.(*structs.SecretMetadataResponse)
		if _, ok := f.secrets[a.Path]; ok {
			r.Secret = &structs.Secret{Path: a.Path, CurrentVersion: 3, MaxVersions: 10,
				ModifyTime: time.Now().UnixNano(), Labels: map[string]string{"owner": "team"},
				Versions: []*structs.SecretVersion{{Version: 2, Deleted: true}, {Version: 3}}}
		}
	default:
		return errors.New("unknown method")
	}
	return nil
}

// testAlloc returns an allocation whose task "web" has a vault block and the
// workload identity wi.
func testAlloc(wi string) *structs.Allocation {
	alloc := mock.Alloc()
	task := alloc.Job.TaskGroups[0].Tasks[0]
	task.Vault = &structs.Vault{Policies: []string{"app"}}
	alloc.SignedIdentities = map[string]string{task.Name: wi}
	return alloc
}

type testEnv struct {
	server  *Server
	servers *fakeServers
	logs    *bytes.Buffer
	info    AllocInfo
}

func newTestEnv(t *testing.T, mod func(*Config)) *testEnv {
	t.Helper()
	fs := &fakeServers{
		secrets: map[string]structs.SecretItems{
			"app/db":    {"password": canary, "username": "app"},
			"other/key": {"k": canary},
		},
		grants: map[string][]string{"wi-web": {"app/"}, "wi-other": {"other/"}},
	}
	logs := &bytes.Buffer{}
	reg := secrets.NewRegistry(secrets.NewNorcaProvider(fs, "global"), nil, nil)
	env := &testEnv{servers: fs, logs: logs}
	cfg := Config{
		Logger:     hclog.New(&hclog.LoggerOptions{Level: hclog.Trace, Output: &syncWriter{w: logs}}),
		Reader:     &secrets.VaultReader{Registry: reg, Mapper: secrets.NewVaultMapper(nil)},
		NodeSecret: "node-secret",
		Port:       0,
		AllocInfo: func(allocID, task string) (*structs.Allocation, bool, string) {
			if env.info != nil {
				return env.info(allocID, task)
			}
			return nil, true, ""
		},
	}
	if mod != nil {
		mod(&cfg)
	}
	env.server = New(cfg)
	t.Cleanup(env.server.Shutdown)
	return env
}

type syncWriter struct {
	lock sync.Mutex
	w    *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.w.Write(p)
}

func (e *testEnv) vault(t *testing.T, alloc *structs.Allocation, token string) *vaultapi.Client {
	t.Helper()
	conf := vaultapi.DefaultConfig()
	conf.Address = e.server.VaultAddr(alloc)
	conf.MaxRetries = 0
	c, err := vaultapi.NewClient(conf)
	must.NoError(t, err)
	c.SetToken(token)
	return c
}

func statusOf(err error) int {
	var rerr *vaultapi.ResponseError
	if errors.As(err, &rerr) {
		return rerr.StatusCode
	}
	return 0
}

// TestEndpoint_VaultGoClient reads KV values with the official Vault Go
// client through the endpoint, with a task token, as an application with
// VAULT_ADDR and VAULT_TOKEN would.
func TestEndpoint_VaultGoClient(t *testing.T) {
	ci.Parallel(t)
	env := newTestEnv(t, nil)
	alloc := testAlloc("wi-web")
	task := alloc.Job.TaskGroups[0].Tasks[0].Name

	tokens, err := env.server.Client().DeriveToken(alloc, []string{task})
	must.NoError(t, err)
	token := tokens[task]
	must.StrHasPrefix(t, "nvc.", token)
	_, err = env.server.Client().RenewToken(token, 30)
	must.NoError(t, err)
	vc := env.vault(t, alloc, token)
	must.StrHasPrefix(t, "http://127.0.0.1:", env.server.VaultAddr(alloc))

	// KV v2.
	sec, err := vc.Logical().Read("kv/data/app/db")
	must.NoError(t, err)
	data := sec.Data["data"].(map[string]any)
	must.Eq(t, canary, data["password"])
	must.Eq(t, "3", sec.Data["metadata"].(map[string]any)["version"].(interface{ String() string }).String())
	kv2, err := vc.KVv2("kv").Get(t.Context(), "app/db")
	must.NoError(t, err)
	must.Eq(t, canary, kv2.Data["password"])
	must.Eq(t, 3, kv2.VersionMetadata.Version)
	kv2, err = vc.KVv2("secret").GetVersion(t.Context(), "app/db", 2)
	must.NoError(t, err)
	must.Eq(t, 2, kv2.VersionMetadata.Version)

	// KV v1.
	kv1, err := vc.KVv1("secret").Get(t.Context(), "app/db")
	must.NoError(t, err)
	must.Eq(t, "app", kv1.Data["username"])

	// KV v2 metadata.
	md, err := vc.KVv2("kv").GetMetadata(t.Context(), "app/db")
	must.NoError(t, err)
	must.Eq(t, 3, md.CurrentVersion)
	must.Eq(t, 10, md.MaxVersions)
	must.Eq(t, "team", md.CustomMetadata["owner"])
	must.MapLen(t, 2, md.Versions)
	must.False(t, md.Versions["2"].DeletionTime.IsZero(), must.Sprint("version 2 is deleted"))
	must.True(t, md.Versions["3"].DeletionTime.IsZero())

	// A missing secret: no error and no secret, as from Vault.
	sec, err = vc.Logical().Read("kv/data/app/missing")
	must.NoError(t, err)
	must.Nil(t, sec)

	// Token self operations and health.
	self, err := vc.Auth().Token().LookupSelf()
	must.NoError(t, err)
	must.Eq[any](t, alloc.ID, self.Data["meta"].(map[string]any)["AllocationID"])
	ttl, err := self.TokenTTL()
	must.NoError(t, err)
	must.Positive(t, ttl)
	renewed, err := vc.Auth().Token().RenewSelf(60)
	must.NoError(t, err)
	must.True(t, renewed.Auth.Renewable)
	must.Eq(t, token, renewed.Auth.ClientToken)
	health, err := vc.Sys().Health()
	must.NoError(t, err)
	must.True(t, health.Initialized)
	must.False(t, health.Sealed)

	// The reads were made with the task's workload identity.
	env.servers.lock.Lock()
	must.SliceContains(t, env.servers.audit, "wi-web app/db ok")
	env.servers.lock.Unlock()
}

// TestEndpoint_Unsupported checks the clear errors of everything the
// endpoint doesn't serve.
func TestEndpoint_Unsupported(t *testing.T) {
	ci.Parallel(t)
	env := newTestEnv(t, nil)
	alloc := testAlloc("wi-web")
	task := alloc.Job.TaskGroups[0].Tasks[0].Name
	tokens, err := env.server.Client().DeriveToken(alloc, []string{task})
	must.NoError(t, err)
	vc := env.vault(t, alloc, tokens[task])

	_, err = vc.Logical().Read("pki/issue/web")
	must.Eq(t, http.StatusNotImplemented, statusOf(err))
	must.ErrorContains(t, err, "is not supported by the built-in store")

	_, err = vc.Logical().Read("database/creds/readonly")
	must.Eq(t, http.StatusNotImplemented, statusOf(err))
	must.ErrorContains(t, err, "requires a Vault/OpenBao provider or the built-in dynamic engine (not available yet)")

	_, err = vc.Logical().Write("kv/data/app/db", map[string]any{"data": map[string]any{"x": "y"}})
	must.Eq(t, http.StatusMethodNotAllowed, statusOf(err))
	must.ErrorContains(t, err, "read-only")

	_, err = vc.Logical().List("kv/metadata/app")
	must.Eq(t, http.StatusNotImplemented, statusOf(err))
	must.ErrorContains(t, err, "not supported by the built-in secret store")

	_, err = vc.Sys().ListMounts()
	must.Eq(t, http.StatusNotImplemented, statusOf(err))
	must.ErrorContains(t, err, "not supported by the built-in secret store")

	_, err = vc.Auth().Token().Create(&vaultapi.TokenCreateRequest{})
	must.Eq(t, http.StatusNotImplemented, statusOf(err))
}

// TestEndpoint_TokenScope checks that a token reads only with its own task's
// identity, can't be used from another task's network namespace, and stops
// working when its task stops; denials of the servers are audited there.
func TestEndpoint_TokenScope(t *testing.T) {
	ci.Parallel(t)
	env := newTestEnv(t, nil)
	web := testAlloc("wi-web")
	other := testAlloc("wi-other")
	webTask := web.Job.TaskGroups[0].Tasks[0].Name

	tokens, err := env.server.Client().DeriveToken(web, []string{webTask})
	must.NoError(t, err)
	webToken := tokens[webTask]
	tokens, err = env.server.Client().DeriveToken(other, []string{webTask})
	must.NoError(t, err)
	otherToken := tokens[webTask]
	must.NotEq(t, webToken, otherToken)

	// The other task's token reads with its own identity: denied for the
	// web task's secret, and the servers audit the denial.
	_, err = env.vault(t, other, otherToken).Logical().Read("kv/data/app/db")
	must.Eq(t, http.StatusForbidden, statusOf(err))
	env.servers.lock.Lock()
	must.SliceContains(t, env.servers.audit, "wi-other app/db denied")
	env.servers.lock.Unlock()

	// A forged or altered token is rejected.
	for _, bad := range []string{"nvc." + web.ID + ".d2Vi.AAAA", webToken + "x", "s.vaulttoken"} {
		_, err = env.vault(t, web, bad).Logical().Read("kv/data/app/db")
		must.Eq(t, http.StatusForbidden, statusOf(err), must.Sprint(bad))
	}

	// A task in bridge mode: its token only from its allocation's address.
	env.info = func(allocID, task string) (*structs.Allocation, bool, string) {
		if allocID == web.ID {
			return web, true, "172.26.64.7"
		}
		return nil, true, ""
	}
	_, err = env.vault(t, web, webToken).Logical().Read("kv/data/app/db")
	must.Eq(t, http.StatusForbidden, statusOf(err))
	must.ErrorContains(t, err, "can only be used by its task")

	// A stopped task: its token is revoked.
	env.info = nil
	must.NoError(t, env.server.Client().StopRenewToken(webToken))
	_, err = env.vault(t, web, webToken).Logical().Read("kv/data/app/db")
	must.Eq(t, http.StatusForbidden, statusOf(err))
	must.ErrorContains(t, err, "expired or was revoked")

	// A task that is no longer running, even before its hook revoked it.
	env.info = func(allocID, task string) (*structs.Allocation, bool, string) { return other, false, "" }
	_, err = env.vault(t, other, otherToken).Logical().Read("kv/data/other/key")
	must.Eq(t, http.StatusForbidden, statusOf(err))

	// Neither values nor tokens are logged.
	logs := env.logs.String()
	must.StrContains(t, logs, "request denied")
	must.StrNotContains(t, logs, canary)
	must.StrNotContains(t, logs, webToken)
	must.StrNotContains(t, logs, otherToken)
}

// TestEndpoint_AdoptAfterRestart checks that a token recovered by the vault
// hook of a running task after an agent restart works again, and that the
// token of a stopped task does not.
func TestEndpoint_AdoptAfterRestart(t *testing.T) {
	ci.Parallel(t)
	first := newTestEnv(t, nil)
	alloc := testAlloc("wi-web")
	task := alloc.Job.TaskGroups[0].Tasks[0].Name
	tokens, err := first.server.Client().DeriveToken(alloc, []string{task})
	must.NoError(t, err)
	token := tokens[task]

	// The restarted agent: same node secret, no live tokens.
	second := newTestEnv(t, nil)
	_, err = second.vault(t, alloc, token).Logical().Read("kv/data/app/db")
	must.Error(t, err)
	must.NoError(t, second.server.Start())
	_, err = second.vault(t, alloc, token).Logical().Read("kv/data/app/db")
	must.Eq(t, http.StatusForbidden, statusOf(err))

	second.info = func(string, string) (*structs.Allocation, bool, string) { return alloc, false, "" }
	_, err = second.server.Client().RenewToken(token, 30)
	must.Error(t, err)

	second.info = func(string, string) (*structs.Allocation, bool, string) { return alloc, true, "" }
	_, err = second.server.Client().RenewToken(token, 30)
	must.NoError(t, err)
	sec, err := second.vault(t, alloc, token).Logical().Read("kv/data/app/db")
	must.NoError(t, err)
	must.Eq(t, canary, sec.Data["data"].(map[string]any)["password"])

	// Another node's secret derives other tokens.
	third := newTestEnv(t, func(c *Config) { c.NodeSecret = "other-node" })
	third.info = second.info
	_, err = third.server.Client().RenewToken(token, 30)
	must.Error(t, err)
}

func TestEndpoint_RateLimit(t *testing.T) {
	ci.Parallel(t)
	env := newTestEnv(t, func(c *Config) { c.RateLimit, c.RateBurst = 0.001, 2 })
	alloc := testAlloc("wi-web")
	task := alloc.Job.TaskGroups[0].Tasks[0].Name
	tokens, err := env.server.Client().DeriveToken(alloc, []string{task})
	must.NoError(t, err)
	vc := env.vault(t, alloc, tokens[task])
	for i := 0; i < 2; i++ {
		_, err = vc.Logical().Read("kv/data/app/db")
		must.NoError(t, err)
	}
	_, err = vc.Logical().Read("kv/data/app/db")
	must.Eq(t, http.StatusTooManyRequests, statusOf(err))
}

func TestEndpoint_BridgeAddress(t *testing.T) {
	ci.Parallel(t)
	env := newTestEnv(t, func(c *Config) { c.Bridge = true; c.BridgeSubnet = "10.99.0.0/24" })
	must.NoError(t, env.server.Start())
	alloc := testAlloc("wi-web")
	alloc.Job.TaskGroups[0].Networks = []*structs.NetworkResource{{Mode: "bridge"}}
	must.StrHasPrefix(t, "http://10.99.0.1:", env.server.VaultAddr(alloc))
	gw, err := env.server.bridgeGateway()
	must.NoError(t, err)
	must.Eq(t, "10.99.0.1", gw.String())

	// Without the bridge (not created on this host), the token is not
	// handed out yet and the task's vault hook retries.
	_, err = env.server.Client().DeriveToken(alloc, []string{alloc.Job.TaskGroups[0].Tasks[0].Name})
	must.Error(t, err)
	must.True(t, structs.IsRecoverable(err))
}

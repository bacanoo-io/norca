// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	vaultapi "github.com/hashicorp/vault/api"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/client/secrets"
	cstate "github.com/hashicorp/nomad/client/state"
	"github.com/hashicorp/nomad/client/taskenv"
	"github.com/hashicorp/nomad/client/vaultcompat"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

// compatServers serves Secret.Read and Secret.ReadMetadata for the workload
// identity "wi-web" and the secret "app/db".
type compatServers struct {
	lock    sync.Mutex
	version uint64
	items   structs.SecretItems
	index   uint64
	paths   []string
}

func (f *compatServers) set(items structs.SecretItems) {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.version++
	f.index++
	f.items = items
}

func (f *compatServers) RPC(method string, args, reply any) error {
	switch method {
	case structs.SecretReadRPCMethod:
		a := args.(*structs.SecretReadRequest)
		f.lock.Lock()
		defer f.lock.Unlock()
		f.paths = append(f.paths, a.Path)
		if a.AuthToken != "wi-web" {
			return structs.ErrPermissionDenied
		}
		if a.Path != "app/db" {
			return errors.New("RPC Error:: 404,secret not found")
		}
		r := reply.(*structs.SecretReadResponse)
		r.Secret = &structs.Secret{Path: a.Path, CurrentVersion: f.version}
		r.Items = f.items.Copy()
		r.Version = f.version
	case structs.SecretReadMetadataRPCMethod:
		a := args.(*structs.SecretReadMetadataRequest)
		deadline := time.Now().Add(a.MaxQueryTime)
		for {
			f.lock.Lock()
			index, version := f.index, f.version
			f.lock.Unlock()
			if index > a.MinQueryIndex || !time.Now().Before(deadline) {
				r := reply.(*structs.SecretMetadataResponse)
				r.Index = index
				r.Secret = &structs.Secret{Path: a.Path, CurrentVersion: version}
				return nil
			}
			time.Sleep(20 * time.Millisecond)
		}
	default:
		return errors.New("unknown method")
	}
	return nil
}

// TestTaskRunner_VaultCompat runs an unmodified job with a vault block and a
// Vault secret template on a client without Vault: the template renders the
// KV v2 value from the built-in store, the task gets VAULT_ADDR and a
// VAULT_TOKEN of the client's endpoint (also in secrets/vault_token) with
// which the Vault Go client reads the same value, the value and the token
// never reach the client state, and the token is revoked when the task
// stops.
func TestTaskRunner_VaultCompat(t *testing.T) {
	ci.Parallel(t)

	alloc := mock.BatchAlloc()
	alloc.Job.TaskGroups[0].Count = 1
	task := alloc.Job.TaskGroups[0].Tasks[0]
	task.Driver = "mock_driver"
	task.Config = map[string]interface{}{"run_for": "60s"}
	task.Vault = &structs.Vault{Policies: []string{"app-read"}, Env: true, ChangeMode: structs.VaultChangeModeRestart}
	task.Templates = []*structs.Template{{
		EmbeddedTmpl: `{{ with secret "kv/data/app/db" }}password={{ .Data.data.password }} version={{ .Data.metadata.version }}{{ end }}`,
		DestPath:     "secrets/app.conf",
		ChangeMode:   structs.TemplateChangeModeNoop,
	}}
	alloc.SignedIdentities = map[string]string{task.Name: "wi-web"}

	servers := &compatServers{}
	servers.set(structs.SecretItems{"password": secretTestCanary})

	conf, cleanup := secretTestTaskRunnerConfig(t, alloc, task)
	defer cleanup()
	conf.ClientConfig.Secrets = &config.SecretsConfig{}
	must.True(t, conf.ClientConfig.VaultCompatEnabled())
	logs := &bytes.Buffer{}
	var logLock sync.Mutex
	conf.Logger = hclog.New(&hclog.LoggerOptions{Level: hclog.Trace, Output: &lockedWriter{w: logs, lock: &logLock}})
	stateDir := t.TempDir()
	db, err := cstate.NewBoltStateDB(conf.Logger, stateDir)
	must.NoError(t, err)
	conf.StateDB = db

	registry := secrets.NewRegistry(secrets.NewNorcaProvider(servers, "global"), nil, nil)
	conf.SecretProviders = registry
	endpoint := vaultcompat.New(vaultcompat.Config{
		Logger:     conf.Logger,
		Reader:     &secrets.VaultReader{Registry: registry, Mapper: secrets.NewVaultMapper(nil)},
		NodeSecret: conf.ClientConfig.Node.SecretID,
	})
	defer endpoint.Shutdown()
	conf.Vault = endpoint.Client()

	tr, err := NewTaskRunner(conf)
	must.NoError(t, err)
	go tr.Run()
	defer tr.Kill(context.Background(), structs.NewTaskEvent("cleanup"))
	testWaitForTaskToStart(t, tr)

	// The template rendered the KV v2 shape from the built-in secret
	// app/db.
	data, err := os.ReadFile(filepath.Join(conf.TaskDir.Dir, "secrets", "app.conf"))
	must.NoError(t, err)
	must.Eq(t, "password="+secretTestCanary+" version=1", string(data))
	servers.lock.Lock()
	must.SliceContains(t, servers.paths, "app/db")
	servers.lock.Unlock()

	// VAULT_ADDR and VAULT_TOKEN, and the token file.
	env := tr.envBuilder.Build().All()
	token := env[taskenv.VaultToken]
	must.StrHasPrefix(t, "nvc.", token)
	must.Eq(t, endpoint.VaultAddr(alloc), env["VAULT_ADDR"])
	must.StrHasPrefix(t, "http://127.0.0.1:", env["VAULT_ADDR"])
	file, err := os.ReadFile(filepath.Join(conf.TaskDir.SecretsDir, "vault_token"))
	must.NoError(t, err)
	must.Eq(t, token, string(file))
	_, err = os.Stat(filepath.Join(conf.TaskDir.PrivateDir, "vault_token"))
	must.True(t, os.IsNotExist(err), must.Sprint("no private copy of the token"))

	// The application's Vault Go client reads the same value.
	vconf := vaultapi.DefaultConfig()
	vconf.Address = env["VAULT_ADDR"]
	vconf.MaxRetries = 0
	vc, err := vaultapi.NewClient(vconf)
	must.NoError(t, err)
	vc.SetToken(token)
	kv, err := vc.KVv2("kv").Get(context.Background(), "app/db")
	must.NoError(t, err)
	must.Eq(t, secretTestCanary, kv.Data["password"])

	// The policies have no counterpart: a warning event.
	found := false
	for _, e := range tr.TaskState().Events {
		if e.Type == vaultCompatTaskEventType && strings.Contains(e.DisplayMessage, "app-read") {
			found = true
		}
	}
	must.True(t, found, must.Sprint("the vault policies warning event"))

	// Neither the value nor the token is in the job, the allocation, the
	// task state, the local state, the state database or the logs.
	outputs := map[string]any{
		"job":         alloc.Job,
		"alloc":       tr.Alloc(),
		"task state":  tr.TaskState(),
		"local state": tr.localState,
	}
	for name, out := range outputs {
		b, err := json.Marshal(out)
		must.NoError(t, err)
		must.StrNotContains(t, string(b), secretTestCanary, must.Sprint(name))
		must.StrNotContains(t, string(b), token, must.Sprint(name))
	}

	// The task stops: the token is revoked.
	tr.Kill(context.Background(), structs.NewTaskEvent("stop"))
	testutil.WaitForResult(func() (bool, error) {
		_, err := vc.KVv2("kv").Get(context.Background(), "app/db")
		var rerr *vaultapi.ResponseError
		if errors.As(err, &rerr) && rerr.StatusCode == http.StatusForbidden {
			return true, nil
		}
		return false, err
	}, func(err error) { t.Fatalf("the token still works after the task stopped: %v", err) })

	must.NoError(t, db.Close())
	dbFiles, err := filepath.Glob(filepath.Join(stateDir, "*"))
	must.NoError(t, err)
	must.SliceNotEmpty(t, dbFiles)
	for _, f := range dbFiles {
		b, err := os.ReadFile(f)
		must.NoError(t, err)
		must.False(t, bytes.Contains(b, []byte(secretTestCanary)), must.Sprintf("client state %s", f))
		must.False(t, bytes.Contains(b, []byte(token)), must.Sprintf("token in client state %s", f))
	}
	logLock.Lock()
	defer logLock.Unlock()
	must.StrNotContains(t, logs.String(), secretTestCanary)
	must.StrNotContains(t, logs.String(), token)
}

// TestTaskRunner_VaultCompat_Template checks the re-render of a Vault secret
// template when the built-in secret changes, and the clear failure of a
// dynamic engine path.
func TestTaskRunner_VaultCompat_Template(t *testing.T) {
	ci.Parallel(t)

	alloc := mock.BatchAlloc()
	task := alloc.Job.TaskGroups[0].Tasks[0]
	task.Driver = "mock_driver"
	task.Config = map[string]interface{}{"run_for": "60s"}
	task.Templates = []*structs.Template{{
		EmbeddedTmpl: `{{ with secret "secret/app/db" }}{{ .Data.password }}{{ end }}`,
		DestPath:     "local/app.conf",
		ChangeMode:   structs.TemplateChangeModeNoop,
	}}
	alloc.SignedIdentities = map[string]string{task.Name: "wi-web"}
	servers := &compatServers{}
	servers.set(structs.SecretItems{"password": "one"})

	conf, cleanup := secretTestTaskRunnerConfig(t, alloc, task)
	defer cleanup()
	conf.ClientConfig.Secrets = &config.SecretsConfig{}
	conf.SecretProviders = secrets.NewRegistry(secrets.NewNorcaProvider(servers, "global"), nil, nil)
	tr, err := NewTaskRunner(conf)
	must.NoError(t, err)
	go tr.Run()
	defer tr.Kill(context.Background(), structs.NewTaskEvent("cleanup"))
	testWaitForTaskToStart(t, tr)

	dest := filepath.Join(conf.TaskDir.Dir, "local", "app.conf")
	data, err := os.ReadFile(dest)
	must.NoError(t, err)
	must.Eq(t, "one", string(data))

	servers.set(structs.SecretItems{"password": "two"})
	testutil.WaitForResultUntil(10*time.Second, func() (bool, error) {
		data, _ := os.ReadFile(dest)
		return string(data) == "two", nil
	}, func(error) { t.Fatal("the template was not rendered again within 10s") })

	// A dynamic engine path fails the task with the message of the plan.
	alloc2 := mock.BatchAlloc()
	task2 := alloc2.Job.TaskGroups[0].Tasks[0]
	task2.Driver = "mock_driver"
	task2.Config = map[string]interface{}{"run_for": "60s"}
	task2.Templates = []*structs.Template{{
		EmbeddedTmpl: `{{ with secret "database/creds/readonly" }}{{ .Data.username }}{{ end }}`,
		DestPath:     "local/db.conf",
	}}
	conf2, cleanup2 := secretTestTaskRunnerConfig(t, alloc2, task2)
	defer cleanup2()
	conf2.ClientConfig.Secrets = &config.SecretsConfig{}
	conf2.SecretProviders = conf.SecretProviders
	tr2, err := NewTaskRunner(conf2)
	must.NoError(t, err)
	go tr2.Run()
	defer tr2.Kill(context.Background(), structs.NewTaskEvent("cleanup"))
	testutil.WaitForResult(func() (bool, error) {
		for _, e := range tr2.TaskState().Events {
			if strings.Contains(e.DisplayMessage, "requires a Vault/OpenBao provider or the built-in dynamic engine") {
				return true, nil
			}
		}
		return false, errors.New("no event")
	}, func(err error) { t.Fatalf("dynamic path: %v (events %v)", err, tr2.TaskState().Events) })
}

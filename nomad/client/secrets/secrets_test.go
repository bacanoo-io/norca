// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

type fakeProvider struct{ name string }

func (f *fakeProvider) Name() string               { return f.name }
func (f *fakeProvider) Type() string               { return "fake" }
func (f *fakeProvider) Capabilities() Capabilities { return Capabilities{} }
func (f *fakeProvider) Fetch(context.Context, *Request) (*Secret, error) {
	return nil, NewError(f.name, KindUnavailable, errors.New("unreachable"))
}

func TestRegistry_ResolveOrder(t *testing.T) {
	ci.Parallel(t)

	norca := NewNorcaProvider(nil, "global")
	vault := &fakeProvider{name: "vault"}
	bao := &fakeProvider{name: "bao"}

	reg := NewRegistry(norca, nil, nil)
	resolve := func(r *Registry, l *Lookup) string {
		t.Helper()
		res, err := r.Resolve(l)
		must.NoError(t, err)
		return res.String()
	}

	// Nothing defines a provider: the built-in store.
	must.Eq(t, "provider norca (fallback)", resolve(reg, &Lookup{Path: "app/db"}))
	// Vault-style lookups without a Vault integration fall back too.
	must.Eq(t, "provider norca (fallback)", resolve(reg, &Lookup{Path: "kv/data/app", VaultStyle: true}))

	reg = NewRegistry(norca, vault, []*config.SecretsRoute{
		{Prefix: "kv/", Provider: "vault"},
		{Prefix: "kv/norca/", Provider: "norca"},
	})
	reg.Register(bao)

	must.Eq(t, "provider bao (explicit)", resolve(reg, &Lookup{Explicit: "bao", Path: "kv/x", GroupDefault: "norca"}))
	must.Eq(t, "provider vault (route)", resolve(reg, &Lookup{Path: "kv/x", GroupDefault: "bao"}))
	must.Eq(t, "provider norca (route)", resolve(reg, &Lookup{Path: "kv/norca/x"}))
	must.Eq(t, "provider bao (group default)", resolve(reg, &Lookup{Path: "app", GroupDefault: "bao", JobDefault: "vault"}))
	must.Eq(t, "provider vault (job default)", resolve(reg, &Lookup{Path: "app", JobDefault: "vault", NamespaceDefault: "bao"}))
	must.Eq(t, "provider bao (namespace default)", resolve(reg, &Lookup{Path: "app", NamespaceDefault: "bao"}))
	must.Eq(t, "provider vault (vault integration)", resolve(reg, &Lookup{Path: "secret/app", VaultStyle: true}))
	must.Eq(t, "provider norca (fallback)", resolve(reg, &Lookup{Path: "app"}))

	// A defined provider that this client doesn't have fails; it never
	// falls back to the built-in store.
	for _, l := range []*Lookup{
		{Explicit: "aws", Path: "app"},
		{Path: "app", NamespaceDefault: "aws"},
		{Path: "app", JobDefault: "aws"},
	} {
		_, err := NewRegistry(norca, nil, nil).Resolve(l)
		must.Error(t, err)
		must.StrContains(t, err.Error(), `provider "aws"`)
		must.False(t, IsRecoverable(err))
	}

	// A defined provider that is unreachable fails with its own error.
	res, err := reg.Resolve(&Lookup{Path: "app", NamespaceDefault: "bao"})
	must.NoError(t, err)
	_, err = res.Provider.Fetch(context.Background(), &Request{Path: "app"})
	must.EqError(t, err, "provider bao: unreachable")
	must.True(t, IsRecoverable(err))
}

func TestLookupFor(t *testing.T) {
	ci.Parallel(t)
	job := &structs.Job{
		Secrets:    &structs.SecretsConfig{DefaultProvider: "a", NamespaceDefaultProvider: "b"},
		TaskGroups: []*structs.TaskGroup{{Name: "g", Secrets: &structs.SecretsConfig{DefaultProvider: "c"}}},
	}
	must.Eq(t, &Lookup{Explicit: "x", Path: "p", GroupDefault: "c", JobDefault: "a", NamespaceDefault: "b"},
		LookupFor(job, "g", "x", "p"))
	must.Eq(t, &Lookup{Path: "p"}, LookupFor(nil, "g", "", "p"))
}

func TestRender(t *testing.T) {
	ci.Parallel(t)
	items := map[string]string{"username": "app", "password": `p"a$s\w` + "\nx"}

	out, err := Render(&structs.TaskSecretFile{Format: "env"}, items)
	must.NoError(t, err)
	must.Eq(t, "password=\"p\\\"a\\$s\\\\w\\nx\"\nusername=\"app\"\n", string(out))

	out, err = Render(&structs.TaskSecretFile{Format: "json"}, items)
	must.NoError(t, err)
	var decoded map[string]string
	must.NoError(t, json.Unmarshal(out, &decoded))
	must.Eq(t, items, decoded)

	out, err = Render(&structs.TaskSecretFile{Format: "raw:username"}, items)
	must.NoError(t, err)
	must.Eq(t, "app", string(out))
	_, err = Render(&structs.TaskSecretFile{Format: "raw:nope"}, items)
	must.EqError(t, err, `the secret has no item "nope"`)

	out, err = Render(&structs.TaskSecretFile{Format: "template", Template: "{{ .username }}:{{ .password }}"}, items)
	must.NoError(t, err)
	must.Eq(t, "app:"+items["password"], string(out))
	_, err = Render(&structs.TaskSecretFile{Format: "template", Template: "{{ .missing }}"}, items)
	must.Error(t, err)
	must.StrNotContains(t, err.Error(), "app")

	env, err := EnvValues(map[string]string{"DB_USER": "username"}, items)
	must.NoError(t, err)
	must.Eq(t, map[string]string{"DB_USER": "app"}, env)
	_, err = EnvValues(map[string]string{"X": "nope"}, items)
	must.Error(t, err)

	must.Eq(t, Fingerprint(items), Fingerprint(map[string]string{"password": items["password"], "username": "app"}))
	must.NotEq(t, Fingerprint(items), Fingerprint(map[string]string{"username": "app"}))
}

func TestVaultSecret(t *testing.T) {
	ci.Parallel(t)

	s, err := vaultSecret(map[string]any{
		"data":     map[string]any{"password": "pw", "port": json.Number("5432")},
		"metadata": map[string]any{"version": json.Number("4")},
	})
	must.NoError(t, err)
	must.Eq(t, map[string]string{"password": "pw", "port": "5432"}, s.Items)
	must.Eq(t, "4", s.Version)

	s, err = vaultSecret(map[string]any{"password": "pw"})
	must.NoError(t, err)
	must.Eq(t, map[string]string{"password": "pw"}, s.Items)
	must.Eq(t, "", s.Version)

	// The KV v2 response of a deleted version.
	_, err = vaultSecret(map[string]any{"data": nil, "metadata": map[string]any{}})
	must.Eq(t, KindNotFound, KindOf(err))

	p := NewVaultProvider(nil)
	must.Nil(t, p)
}

// fakeRPC serves Secret.Read and Secret.ReadMetadata from memory, with
// blocking metadata queries.
type fakeRPC struct {
	lock    sync.Mutex
	cond    *sync.Cond
	index   uint64
	version uint64
	items   structs.SecretItems
	err     error
	tokens  []string
}

func newFakeRPC() *fakeRPC {
	f := &fakeRPC{}
	f.cond = sync.NewCond(&f.lock)
	return f
}

func (f *fakeRPC) set(items structs.SecretItems) {
	f.lock.Lock()
	f.index++
	f.version++
	f.items = items
	f.cond.Broadcast()
	f.lock.Unlock()
}

func (f *fakeRPC) RPC(method string, args, reply any) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	if f.err != nil {
		return f.err
	}
	switch method {
	case structs.SecretReadRPCMethod:
		a := args.(*structs.SecretReadRequest)
		f.tokens = append(f.tokens, a.AuthToken)
		r := reply.(*structs.SecretReadResponse)
		if f.items != nil {
			r.Secret = &structs.Secret{Path: a.Path, CurrentVersion: f.version}
			r.Items = f.items.Copy()
			r.Version = f.version
		}
	case structs.SecretReadMetadataRPCMethod:
		a := args.(*structs.SecretReadMetadataRequest)
		deadline := time.Now().Add(a.MaxQueryTime)
		for f.index <= a.MinQueryIndex && time.Now().Before(deadline) {
			go func() { time.Sleep(10 * time.Millisecond); f.cond.Broadcast() }()
			f.cond.Wait()
		}
		r := reply.(*structs.SecretMetadataResponse)
		r.Index = f.index
		if f.items != nil {
			r.Secret = &structs.Secret{Path: a.Path, CurrentVersion: f.version}
		}
	default:
		return errors.New("unknown method")
	}
	return nil
}

func TestNorcaProvider(t *testing.T) {
	ci.Parallel(t)
	rpc := newFakeRPC()
	p := NewNorcaProvider(rpc, "global")
	id := &Identity{Token: "wi-token", Namespace: "default"}
	req := &Request{Path: "app/db", Identity: id}
	ctx := context.Background()

	_, err := p.Fetch(ctx, req)
	must.Eq(t, KindNotFound, KindOf(err))

	rpc.set(structs.SecretItems{"password": "one"})
	s, err := p.Fetch(ctx, req)
	must.NoError(t, err)
	must.Eq(t, "1", s.Version)
	must.Eq(t, "one", s.Items["password"])
	must.Eq(t, []string{"wi-token", "wi-token"}, rpc.tokens)

	// Watch returns when the version changes.
	done := make(chan string, 1)
	go func() {
		v, err := p.Watch(ctx, req, "1")
		must.NoError(t, err)
		done <- v
	}()
	time.Sleep(50 * time.Millisecond)
	rpc.set(structs.SecretItems{"password": "two"})
	select {
	case v := <-done:
		must.Eq(t, "2", v)
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not return")
	}

	_, err = p.Fetch(ctx, &Request{Path: "../x", Identity: id})
	must.Eq(t, KindInvalid, KindOf(err))

	for msg, kind := range map[string]ErrorKind{
		structs.ErrPermissionDenied.Error(): KindDenied,
		"RPC Error:: 404,secret not found":  KindNotFound,
		"RPC Error:: 503,sealed":            KindUnavailable,
		"no servers":                        KindUnavailable,
	} {
		rpc.err = errors.New(msg)
		_, err = p.Fetch(ctx, req)
		must.Eq(t, kind, KindOf(err), must.Sprint(msg))
	}
	must.Eq(t, structs.SecretAccessResultDenied, AuditResult(NewError("x", KindDenied, errors.New("d"))))
	must.Eq(t, structs.SecretAccessResultOK, AuditResult(nil))
	must.True(t, p.Capabilities().AuditsReads)
}

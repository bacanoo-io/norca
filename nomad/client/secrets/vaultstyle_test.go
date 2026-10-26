// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secrets

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"text/template"
	"time"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

func TestVaultMapper_DefaultRules(t *testing.T) {
	ci.Parallel(t)
	m := NewVaultMapper(nil)

	for path, want := range map[string]VaultPath{
		"kv/data/app/db":         {Path: "app/db", KVVersion: 2},
		"/kv/data/app/db":        {Path: "app/db", KVVersion: 2},
		"secret/data/app/db":     {Path: "app/db", KVVersion: 2},
		"secret/app/db":          {Path: "app/db", KVVersion: 1},
		"kv/metadata/app/db":     {Path: "app/db", KVVersion: 2, Metadata: true},
		"secret/metadata/app/db": {Path: "app/db", KVVersion: 2, Metadata: true},
	} {
		got, err := m.Map(path)
		must.NoError(t, err, must.Sprint(path))
		must.Eq(t, want, *got, must.Sprint(path))
	}

	// Dynamic engines fail with the message of the plan; never a fallback.
	_, err := m.Map("database/creds/readonly")
	must.ErrorContains(t, err, "requires a Vault/OpenBao provider or the built-in dynamic engine (not available yet)")
	must.True(t, IsUnsupported(err))
	must.False(t, IsRecoverable(err))
	_, err = m.Map("aws/sts/deploy")
	must.ErrorContains(t, err, "dynamic secrets engine")

	// Other engines and unmapped mounts are not supported.
	_, err = m.Map("pki/issue/web")
	must.ErrorContains(t, err, `Vault path "pki/issue/web" is not supported by the built-in store`)
	must.ErrorContains(t, err, "kv/data/")
	must.True(t, IsUnsupported(err))

	// A mount without a secret path, and paths that leave the store's rules.
	_, err = m.Map("kv/data/")
	must.ErrorContains(t, err, "names no secret")
	must.False(t, IsUnsupported(err))
	_, err = m.Map("secret/../x")
	must.ErrorContains(t, err, "invalid secret path")
}

func TestVaultMapper_CustomRules(t *testing.T) {
	ci.Parallel(t)
	rules := []*config.VaultKVRule{
		{Prefix: "kv/data/", Path: "apps/"},
		{Prefix: "kv/data/shared/", Path: "global/"},
		{Prefix: "legacy/", KVVersion: 1, Path: "old/"},
	}
	must.NoError(t, config.ValidateVaultKVRules(rules))
	m := NewVaultMapper(rules)

	got, err := m.Map("kv/data/db/pg")
	must.NoError(t, err)
	must.Eq(t, VaultPath{Path: "apps/db/pg", KVVersion: 2}, *got)
	// The longest prefix wins, also for its metadata.
	got, err = m.Map("kv/data/shared/smtp")
	must.NoError(t, err)
	must.Eq(t, VaultPath{Path: "global/smtp", KVVersion: 2}, *got)
	got, err = m.Map("kv/metadata/db/pg")
	must.NoError(t, err)
	must.Eq(t, VaultPath{Path: "apps/db/pg", KVVersion: 2, Metadata: true}, *got)
	got, err = m.Map("legacy/app")
	must.NoError(t, err)
	must.Eq(t, VaultPath{Path: "old/app", KVVersion: 1}, *got)

	// The configured rules replace the defaults.
	_, err = m.Map("secret/app")
	must.True(t, IsUnsupported(err))

	for _, bad := range [][]*config.VaultKVRule{
		{{Prefix: "kv/data"}},
		{{Prefix: "/kv/"}},
		{{Prefix: "kv/", KVVersion: 3}},
		{{Prefix: "kv/", Path: "../x/"}},
		{{Prefix: "kv/", Path: "x"}},
		{{Prefix: "kv/"}, {Prefix: "kv/"}},
	} {
		must.Error(t, config.ValidateVaultKVRules(bad))
	}
}

// recordedKVv2 is a KV v2 read response in the shape of the Vault HTTP API
// documentation ("KV secrets engine - version 2", read secret version).
const recordedKVv2 = `{
  "request_id": "b4c1d9a1-0000-0000-0000-000000000000",
  "lease_id": "",
  "renewable": false,
  "lease_duration": 0,
  "data": {
    "data": {"password": "canary-pw", "username": "app"},
    "metadata": {
      "created_time": "2026-10-20T10:00:00.123456789Z",
      "custom_metadata": {"owner": "team"},
      "deletion_time": "",
      "destroyed": false,
      "version": 3
    }
  },
  "wrap_info": null,
  "warnings": null,
  "auth": null
}`

// recordedKVv1 is a KV v1 read response.
const recordedKVv1 = `{
  "request_id": "b4c1d9a1-0000-0000-0000-000000000001",
  "lease_id": "",
  "renewable": false,
  "lease_duration": 2764800,
  "data": {"password": "canary-pw", "username": "app"},
  "wrap_info": null,
  "warnings": null,
  "auth": null
}`

// TestVaultKV_RecordedShapes checks that templates render the same output
// from the built-in store's responses as from Vault's recorded responses
// (consul-template hands templates the data of the Vault API client's
// secret).
func TestVaultKV_RecordedShapes(t *testing.T) {
	ci.Parallel(t)
	created := time.Date(2026, 10, 20, 10, 0, 0, 123456789, time.UTC)
	sec := &Secret{
		Items:   map[string]string{"password": "canary-pw", "username": "app"},
		Version: "3",
		Metadata: &Metadata{
			Version: 3, CreatedTime: created, CurrentVersion: 3,
			Labels: map[string]string{"owner": "team"},
		},
	}

	render := func(text string, data any) string {
		t.Helper()
		var buf bytes.Buffer
		must.NoError(t, template.Must(template.New("t").Option("missingkey=error").Parse(text)).Execute(&buf, data))
		return buf.String()
	}

	v2, err := vaultapi.ParseSecret(strings.NewReader(recordedKVv2))
	must.NoError(t, err)
	ours := vaultKVFromSecret(sec, 2)
	tmpl := `{{ .Data.data.username }}:{{ .Data.data.password }} v{{ .Data.metadata.version }} ` +
		`{{ .Data.metadata.created_time }} {{ .Data.metadata.custom_metadata.owner }} ` +
		`{{ .Data.metadata.destroyed }} [{{ .Data.metadata.deletion_time }}] {{ .LeaseDuration }}` +
		`{{ range $k, $v := .Data.data }} {{ $k }}={{ $v }}{{ end }}`
	want := render(tmpl, &VaultTemplateSecret{Data: v2.Data, LeaseDuration: v2.LeaseDuration})
	must.Eq(t, want, render(tmpl, &VaultTemplateSecret{Data: ours.Data, LeaseDuration: ours.LeaseDuration}))
	must.StrContains(t, want, "app:canary-pw v3 2026-10-20T10:00:00.123456789Z team false [] 0")

	v1, err := vaultapi.ParseSecret(strings.NewReader(recordedKVv1))
	must.NoError(t, err)
	ours = vaultKVFromSecret(sec, 1)
	tmpl = `{{ .Data.username }}:{{ .Data.password }} {{ .LeaseDuration }}`
	must.Eq(t, render(tmpl, &VaultTemplateSecret{Data: v1.Data, LeaseDuration: v1.LeaseDuration}),
		render(tmpl, &VaultTemplateSecret{Data: ours.Data, LeaseDuration: ours.LeaseDuration}))

	// The copy handed to a template is independent of the cached value.
	cp := ours.Copy()
	cp.Data["password"] = "changed"
	must.Eq(t, "canary-pw", ours.Data["password"])
	must.Eq(t, ours.Fingerprint(), vaultKVFromSecret(sec, 1).Fingerprint())
}

func TestParseVaultRef(t *testing.T) {
	ci.Parallel(t)
	p, v, err := ParseVaultRef("/kv/data/app?version=2")
	must.NoError(t, err)
	must.Eq(t, "kv/data/app", p)
	must.Eq(t, uint64(2), v)
	_, _, err = ParseVaultRef("kv/data/app?ttl=1h")
	must.ErrorContains(t, err, "not supported")
}

// TestTemplateSource_Vault serves Vault secret calls from the built-in store
// through the fake RPC: KV v2 and v1 shapes, metadata, change detection,
// and the errors of unsupported paths and writes.
func TestTemplateSource_Vault(t *testing.T) {
	ci.Parallel(t)
	rpc := newFakeRPC()
	rpc.set(structs.SecretItems{"password": "one"})
	reg := NewRegistry(NewNorcaProvider(rpc, "global"), nil, nil)
	src := NewTemplateSource(&TemplateSourceConfig{
		Registry: reg,
		Lookup:   func(explicit, path string) *Lookup { return &Lookup{Explicit: explicit, Path: path} },
		Identity: func() *Identity { return &Identity{Token: "wi", Namespace: "default"} },
		Vault:    &VaultReader{Registry: reg, Mapper: NewVaultMapper(nil)},
		Timeout:  2 * time.Second,
	})
	defer src.Stop()
	must.True(t, src.ServesVault())

	out, err := src.VaultFunc("kv/data/app/db")
	must.NoError(t, err)
	s := out.(*VaultTemplateSecret)
	data := s.Data["data"].(map[string]any)
	must.Eq(t, "one", data["password"])
	must.Eq(t, "1", s.Data["metadata"].(map[string]any)["version"].(interface{ String() string }).String())

	out, err = src.VaultFunc("secret/app/db")
	must.NoError(t, err)
	must.Eq(t, "one", out.(*VaultTemplateSecret).Data["password"])
	must.Eq(t, kvV1LeaseDuration, out.(*VaultTemplateSecret).LeaseDuration)

	out, err = src.VaultFunc("kv/metadata/app/db")
	must.NoError(t, err)
	must.Eq(t, "1", out.(*VaultTemplateSecret).Data["current_version"].(interface{ String() string }).String())

	// A change is signaled and the next call returns the new value.
	rpc.set(structs.SecretItems{"password": "two"})
	select {
	case <-src.Changes():
	case <-time.After(10 * time.Second):
		t.Fatal("no change signaled")
	}
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		out, err := src.VaultFunc("kv/data/app/db")
		return err == nil && out.(*VaultTemplateSecret).Data["data"].(map[string]any)["password"] == "two"
	}), wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))

	_, err = src.VaultFunc("database/creds/ro")
	must.ErrorContains(t, err, "requires a Vault/OpenBao provider or the built-in dynamic engine (not available yet)")
	_, err = src.VaultFunc("pki/issue/web", "common_name=x")
	must.ErrorContains(t, err, "writes")
	_, err = src.VaultListFunc("kv/metadata/")
	must.ErrorContains(t, err, "listing")

	// The paths read from the store are the mapped ones.
	rpc.lock.Lock()
	defer rpc.lock.Unlock()
	must.SliceContains(t, rpc.tokens, "wi")
}

// TestVaultReader_Resolution checks that Vault-style lookups use the
// resolution order: a defined provider this client doesn't have fails, it
// never falls back to the built-in store.
func TestVaultReader_Resolution(t *testing.T) {
	ci.Parallel(t)
	rpc := newFakeRPC()
	reg := NewRegistry(NewNorcaProvider(rpc, "global"), nil, nil)
	r := &VaultReader{Registry: reg, Mapper: NewVaultMapper(nil)}

	vl, err := r.Resolve(&Lookup{Path: "kv/data/app/db"}, 0)
	must.NoError(t, err)
	must.Eq(t, "provider norca (fallback)", vl.Resolution.String())
	must.Eq(t, "app/db", vl.Request(nil).Path)

	_, err = r.Resolve(&Lookup{Path: "kv/data/app/db", NamespaceDefault: "bao"}, 0)
	must.ErrorContains(t, err, `provider "bao"`)

	// Versions only for KV v2 data paths.
	_, err = r.Resolve(&Lookup{Path: "secret/app/db"}, 2)
	must.ErrorContains(t, err, "versions")

	// A missing secret is "not found", recoverable.
	_, err = r.Read(context.Background(), vl, &Identity{Token: "wi"})
	must.Eq(t, KindNotFound, KindOf(err))
}

func TestVaultPolicyWarning(t *testing.T) {
	ci.Parallel(t)
	job := &structs.Job{ID: "web", TaskGroups: []*structs.TaskGroup{{
		Name:  "g",
		Tasks: []*structs.Task{{Name: "t"}, {Name: "u", SecretAccess: []*structs.SecretAccess{{Path: "jobs/web/*"}}}},
	}}}
	must.Eq(t, "", VaultPolicyWarning(job, "g", "t", nil))
	must.StrContains(t, VaultPolicyWarning(job, "g", "t", []string{"b", "a"}), "Vault policies a, b are not used")
	must.Eq(t, "", VaultPolicyWarning(job, "g", "u", []string{"a"}))
}

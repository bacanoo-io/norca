// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package packconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shoenig/test/must"
)

func TestConfig_LoadAndPolicy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	defaultKey := filepath.ToSlash(filepath.Join(t.TempDir(), "default.pub"))
	path := filepath.Join(dir, "config.hcl")
	must.NoError(t, os.WriteFile(path, []byte(`
# hand written
registry "internal" {
  url        = "oci://registry.example.internal:5000/packs"
  plain_http = true
  ca_cert    = "ca.pem"

  trust {
    require_signature = true
    public_keys       = ["keys/internal.pub"]
  }
}

registry "mirror" {
  url = "oci://mirror.example.com"
}

trust {
  require_signature = false
  public_keys       = ["`+defaultKey+`"]
}
`), 0o600))

	cfg, err := Load(path)
	must.NoError(t, err)
	must.Eq(t, []string{"internal", "mirror"}, cfg.RegistryNames())

	internal := cfg.Registry("internal")
	must.NotNil(t, internal)
	opts := cfg.Options(internal)
	must.True(t, opts.PlainHTTP)
	must.Eq(t, filepath.Join(dir, "ca.pem"), opts.CACertFile)
	must.Eq(t, "registry.example.internal:5000", internal.Location().Host)

	p := cfg.TrustPolicy("internal")
	must.True(t, p.RequireSignature)
	must.Eq(t, []string{filepath.Join(dir, "keys", "internal.pub")}, p.KeyFiles)

	p = cfg.TrustPolicy("mirror")
	must.False(t, p.RequireSignature)
	must.Eq(t, []string{defaultKey}, p.KeyFiles)

	p = cfg.TrustPolicy("unknown")
	must.False(t, p.RequireSignature)
}

func TestConfig_Missing(t *testing.T) {
	t.Parallel()
	cfg, err := Load(filepath.Join(t.TempDir(), "none.hcl"))
	must.NoError(t, err)
	must.SliceEmpty(t, cfg.Registries)
	must.False(t, cfg.TrustPolicy("x").RequireSignature)
}

func TestConfig_Invalid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.hcl")
	must.NoError(t, os.WriteFile(path, []byte(`registry "a" { url = "https://example.com" }`), 0o600))
	_, err := Load(path)
	must.ErrorContains(t, err, "oci://")

	must.NoError(t, os.WriteFile(path, []byte("registry \"a\" { url = \"oci://h/r\" }\nregistry \"a\" { url = \"oci://h/s\" }\n"), 0o600))
	_, err = Load(path)
	must.ErrorContains(t, err, "more than once")
}

func TestConfig_SaveRemove(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sub", "config.hcl")

	req := true
	must.NoError(t, SaveRegistry(path, &Registry{
		Name: "internal", URL: "oci://localhost:5000/packs", PlainHTTP: true,
		Trust: &Trust{RequireSignature: &req, PublicKeys: []string{"/k.pub"}},
	}))
	// Add a comment by hand; it must survive later updates.
	b, err := os.ReadFile(path)
	must.NoError(t, err)
	must.NoError(t, os.WriteFile(path, append([]byte("# keep me\n"), b...), 0o600))

	must.NoError(t, SaveRegistry(path, &Registry{Name: "other", URL: "oci://ghcr.io/org"}))
	// Replacing a registry keeps a single definition.
	must.NoError(t, SaveRegistry(path, &Registry{Name: "internal", URL: "oci://localhost:5001/packs", PlainHTTP: true,
		Trust: &Trust{RequireSignature: &req, PublicKeys: []string{"/k.pub"}}}))

	cfg, err := Load(path)
	must.NoError(t, err)
	must.Eq(t, []string{"internal", "other"}, cfg.RegistryNames())
	must.Eq(t, "oci://localhost:5001/packs", cfg.Registry("internal").URL)
	must.True(t, cfg.TrustPolicy("internal").RequireSignature)
	b, err = os.ReadFile(path)
	must.NoError(t, err)
	must.StrContains(t, string(b), "# keep me")

	removed, err := RemoveRegistry(path, "internal")
	must.NoError(t, err)
	must.True(t, removed)
	removed, err = RemoveRegistry(path, "internal")
	must.NoError(t, err)
	must.False(t, removed)
	cfg, err = Load(path)
	must.NoError(t, err)
	must.Eq(t, []string{"other"}, cfg.RegistryNames())

	must.Error(t, SaveRegistry(path, &Registry{Name: "bad", URL: "registry.example.com"}))
}

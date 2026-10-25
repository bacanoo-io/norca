// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package deps

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/shoenig/test/must"

	"github.com/hashicorp/nomad/pack/internal/pkg/helper"
	"github.com/hashicorp/nomad/pack/internal/pkg/loader"
	"github.com/hashicorp/nomad/pack/internal/pkg/oci"
	"github.com/hashicorp/nomad/pack/internal/pkg/oci/ocitest"
	"github.com/hashicorp/nomad/pack/internal/pkg/packconfig"
	"github.com/hashicorp/nomad/pack/internal/pkg/packlock"
	"github.com/hashicorp/nomad/pack/internal/pkg/testfixture"
	"github.com/hashicorp/nomad/pack/internal/testui"
)

// TestVendor_OCIAndLock vendors a dependency from an OCI registry and checks
// the lock file handling. It sets the environment and can't run in parallel.
func TestVendor_OCIAndLock(t *testing.T) {
	ctx := context.Background()
	srv := ocitest.New().Start()
	defer srv.Close()

	// Publish the dependency.
	depDir := testfixture.Clone(t, "v2/test_registry/packs/simple_raw_exec")
	p, err := loader.Load(depDir)
	must.NoError(t, err)
	a, err := oci.Build(depDir, p.Metadata)
	must.NoError(t, err)
	loc := oci.Location{Host: srv.Host(), Repository: "packs/simple_raw_exec"}
	repo, err := (&oci.Options{PlainHTTP: true, NoDockerConfig: true}).Repository(loc)
	must.NoError(t, err)
	must.NoError(t, a.Push(ctx, repo, "0.0.1"))

	// The registry is configured for plain HTTP.
	confFile := filepath.Join(t.TempDir(), "config.hcl")
	must.NoError(t, packconfig.SaveRegistry(confFile, &packconfig.Registry{
		Name: "test", URL: "oci://" + srv.Host() + "/packs", PlainHTTP: true,
	}))
	t.Setenv(packconfig.EnvConfigFile, confFile)

	packDir := t.TempDir()
	must.NoError(t, os.WriteFile(filepath.Join(packDir, "metadata.hcl"), []byte(fmt.Sprintf(`
app {
  url = ""
}
pack {
  name        = "parent"
  description = "parent"
  version     = "0.1.0"
}
dependency "simple_raw_exec" {
  source = "oci://%s/packs/simple_raw_exec"
  ref    = "0.0.1"
}
`, srv.Host())), 0o644))

	var out bytes.Buffer
	uiCtx, cancel := helper.WithInterrupt(context.Background())
	defer cancel()
	ui := testui.NonInteractiveTestUI(uiCtx, &out, &out)

	must.NoError(t, Vendor(ctx, ui, packDir))
	_, err = os.Stat(filepath.Join(packDir, "deps", "simple_raw_exec", "metadata.hcl"))
	must.NoError(t, err)

	lock, err := packlock.Read(packDir)
	must.NoError(t, err)
	must.Eq(t, "parent", lock.Pack.Name)
	dep := lock.Dependency("simple_raw_exec")
	must.NotNil(t, dep)
	must.Eq(t, a.Digest(), dep.ManifestDigest)
	must.Eq(t, a.TreeDigest(), dep.Digest)
	first, err := os.ReadFile(packlock.Path(packDir))
	must.NoError(t, err)

	// Vendoring again gives the identical lock file.
	must.NoError(t, Vendor(ctx, ui, packDir))
	second, err := os.ReadFile(packlock.Path(packDir))
	must.NoError(t, err)
	must.Eq(t, string(first), string(second))

	// The same version with different content is refused...
	must.NoError(t, os.WriteFile(filepath.Join(depDir, "README.md"), []byte("changed"), 0o644))
	changed, err := oci.Build(depDir, p.Metadata)
	must.NoError(t, err)
	must.NoError(t, changed.Push(ctx, repo, "0.0.1"))
	err = Vendor(ctx, ui, packDir)
	must.ErrorContains(t, err, "--update-lock")

	// ...unless the update is requested.
	must.NoError(t, VendorWithOptions(ctx, ui, packDir, Options{UpdateLock: true}))
	lock, err = packlock.Read(packDir)
	must.NoError(t, err)
	must.Eq(t, changed.Digest(), lock.Dependency("simple_raw_exec").ManifestDigest)
}

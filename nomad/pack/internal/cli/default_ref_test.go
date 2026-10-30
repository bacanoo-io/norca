// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package cli

import (
	"os"
	"path"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/command/agent"
	"github.com/shoenig/test/must"

	ct "github.com/hashicorp/nomad/pack/internal/cli/testhelper"
	"github.com/hashicorp/nomad/pack/internal/pkg/caching"
	"github.com/hashicorp/nomad/pack/internal/pkg/helper/filesystem"
	"github.com/hashicorp/nomad/pack/internal/pkg/logging"
	"github.com/hashicorp/nomad/pack/internal/runner/job"
)

// TestCLI_PinnedRegistryRef runs a pack of a registry that was added with a
// pinned ref without --ref: the pack is used at the pinned ref, and the
// deployment and the release are named after the pack.
func TestCLI_PinnedRegistryRef(t *testing.T) {
	_, cacheDir := isolateSupplyChain(t)

	// A registry added with --ref=<testRef>: the packs are only cached at it.
	registryName := uniqueTestRegistryName()
	must.NoError(t, filesystem.CopyDir(
		getTestPackPath(t, testPack),
		path.Join(cacheDir, registryName, testRef, testPack+"@"+testRef),
		false,
		logging.Default(),
	))
	regFlag := "--registry=" + registryName

	ct.HTTPTest(t, ct.WithDefaultConfig(), func(s *agent.TestAgent) {
		client, err := ct.NewTestClient(s)
		must.NoError(t, err)

		// An explicit ref keeps its meaning: the pack is not cached at latest.
		r := runTestPackCmd(t, s, []string{"plan", testPack, regFlag, "--ref=latest"})
		must.StrContains(t, r.cmdOut.String(), "Failed To Find Pack")

		// --detach: the test does not depend on the task starting.
		r = runTestPackCmd(t, s, []string{"run", testPack, regFlag, "--detach"})
		expectGoodPackDeploy(t, r)
		must.StrContains(t, r.cmdOut.String(), "Use "+testPack+" to manage")

		j, _, err := client.Jobs().Info(testPack, nil)
		must.NoError(t, err)
		must.Eq(t, testPack, j.Meta[job.PackDeploymentNameKey])
		must.Eq(t, testRef, j.Meta[job.PackRefKey])
		must.Eq(t, testPack, j.Meta[api.ReleaseMetaRelease])

		r = runTestPackCmd(t, s, []string{"status", testPack, regFlag})
		expectExit(t, r, 0)
		must.StrContains(t, r.cmdOut.String(), "Deployed revision")

		r = runTestPackCmd(t, s, []string{"destroy", testPack, regFlag})
		expectExit(t, r, 0)
		_, _, err = client.Jobs().Info(testPack, nil)
		must.ErrorContains(t, err, "404")
	})
}

// TestCLI_DefaultRefRecord uses the ref recorded by the last registry add
// when the packs are cached at several refs.
func TestCLI_DefaultRefRecord(t *testing.T) {
	_, cacheDir := isolateSupplyChain(t)

	registryName := uniqueTestRegistryName()
	for _, ref := range []string{caching.DefaultRef, testRef} {
		must.NoError(t, filesystem.CopyDir(
			getTestPackPath(t, testPack),
			path.Join(cacheDir, registryName, ref, testPack+"@"+ref),
			false,
			logging.Default(),
		))
	}
	cfg := &caching.PackConfig{Registry: registryName, Name: testPack}
	cfg.Init()
	must.Eq(t, caching.DefaultRef, cfg.Ref)

	must.NoError(t, os.WriteFile(path.Join(cacheDir, registryName, caching.DefaultRefFile), []byte(testRef+"\n"), 0o644))
	cfg = &caching.PackConfig{Registry: registryName, Name: testPack}
	cfg.Init()
	must.Eq(t, testRef, cfg.Ref)
	must.Eq(t, testPack, defaultDeploymentName(cfg))

	cfg = &caching.PackConfig{Registry: registryName, Name: testPack, Ref: caching.DefaultRef}
	cfg.Init()
	must.Eq(t, caching.DefaultRef, cfg.Ref)
	must.Eq(t, testPack+"@latest", defaultDeploymentName(cfg))

	// The registry list is not disturbed by the record.
	r := runPackCmd(t, []string{"registry", "list"})
	expectExit(t, r, 0)
	must.StrContains(t, r.cmdOut.String(), registryName)
}

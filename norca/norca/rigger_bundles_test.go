// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/testutil"
	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/bundle"
)

// A bundle imported into the cluster is unpacked into the cache of the
// server by the commit of its module, and a server with an empty cache (a
// new leader) unpacks it from the bus before it evaluates anything.
func TestRiggerBundles_Unpack(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, enableBus(t))
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)
	waitBus(t, func() (bool, error) { return s.bus.JetStreamReady(), nil })

	root := fstest.MapFS{
		"modules/web/metadata.hcl": {Data: []byte("module {\n  name    = \"web\"\n  version = \"1.0.0\"\n}\n")},
	}

	eng, err := rigger.New()
	must.NoError(t, err)

	digest, err := eng.ModuleDigest(t.Context(), root, "modules/web")
	must.NoError(t, err)

	commit := strings.Repeat("b", 40)

	var buf bytes.Buffer
	must.NoError(t, bundle.Write(&buf, "shop", []rigger.SourcedModule{{
		Source: "git.example.com/acme/modules//modules/web", Version: "1.0.0", Path: "modules/web",
		Commit: commit, Digest: digest, Dirs: []string{"modules/web"}, Root: root,
	}}))

	b, err := s.importRiggerBundle(t.Context(), buf.Bytes())
	must.NoError(t, err)
	must.Eq(t, "shop", b.Stack)

	r, err := s.riggerRegistriesOf()
	must.NoError(t, err)

	_, err = os.Stat(filepath.Join(r.fetcher.BundleDir(commit), "modules", "web", "metadata.hcl"))
	must.NoError(t, err, must.Sprint("the bundle is unpacked into the cache of the server that imported it"))

	// Another cache, as a new leader has.
	other := newRiggerRegistries(s, t.TempDir())
	must.NoError(t, other.unpackBundles(t.Context()))

	_, err = os.Stat(filepath.Join(other.fetcher.BundleDir(commit), "modules", "web", "metadata.hcl"))
	must.NoError(t, err, must.Sprint("an empty cache gets the bundle from the bus"))

	// Too large to be sent.
	_, err = s.importRiggerBundle(t.Context(), make([]byte, riggerBundleMaxBytes+1))
	must.ErrorContains(t, err, "more than")
}

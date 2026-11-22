// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"bytes"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/testutil"
	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/bundle"
)

// The servers check the published stack of a part by the digest of a stack.
func TestRiggerBundles_Part(t *testing.T) {
	ci.Parallel(t)

	s, cleanup := TestServer(t, nil)
	t.Cleanup(cleanup)
	testutil.WaitForLeader(t, s.RPC)

	root := fstest.MapFS{
		"stacks/data/metadata.hcl": {Data: []byte("stack {\n  name    = \"data\"\n  version = \"1.0.0\"\n}\n")},
		"stacks/data/stack.hcl":    {Data: []byte("rigger {\n  name = \"data\"\n}\n")},
	}

	eng, err := rigger.New()
	must.NoError(t, err)

	st, diags := eng.ReadStack(t.Context(), root, "stacks/data")
	must.False(t, diags.HasErrors(), must.Sprint(diags))

	digest, err := st.Digest()
	must.NoError(t, err)

	var buf bytes.Buffer
	must.NoError(t, bundle.Write(&buf, "site", []rigger.SourcedModule{{
		Source: "example.com/acme/stacks//stacks/data", Version: "1.0.0", Repository: "example.com/acme/stacks", Stack: true,
		Path: "stacks/data", Commit: strings.Repeat("e", 40), Digest: digest, Dirs: []string{"stacks/data"}, Root: root,
	}}))

	_, err = s.checkRiggerBundle(t.Context(), buf.Bytes())
	must.NoError(t, err)
}

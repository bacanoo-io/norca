// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/sign"
	"github.com/bacanoo-io/norca/rigger/source"
)

// hostFetcher is the fetcher of a host with keys of its own for a source;
// it keeps the requests it was asked.
type hostFetcher struct {
	repoFetcher

	keys  [][]string
	asked *[]source.Request
}

func (f hostFetcher) Fetch(ctx context.Context, req source.Request) (source.Fetched, error) {
	*f.asked = append(*f.asked, req)

	got, err := f.repoFetcher.Fetch(ctx, req)
	got.TrustedKeys = f.keys

	return got, err
}

func hostKeys(t *testing.T) [][]string {
	t.Helper()

	key, err := sign.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	pub, err := sign.MarshalPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	return [][]string{{string(pub)}}
}

// The keys of a host are needed whatever the stack trusts: a part, a
// published stack and a module are refused unsigned. A fetch of a locked
// version names the digest of the lock, for a host that allows by it.
func TestEngine_HostTrust(t *testing.T) {
	t.Parallel()

	newEngine := func(tree fstest.MapFS, keys [][]string, asked *[]source.Request) *rigger.Engine {
		e, err := rigger.New(rigger.WithLogger(slog.New(slog.DiscardHandler)),
			rigger.WithFetcher(hostFetcher{repoFetcher{tree: tree, version: publishedVersion}, keys, asked}))
		if err != nil {
			t.Fatal(err)
		}

		return e
	}

	var asked []source.Request

	files := stackFiles(siteStack)

	_, lock, diags := newEngine(publishedData(), nil, &asked).SyncStack(t.Context(), files, "s", false)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	files["s/rigger.lock.hcl"] = &fstest.MapFile{Data: lock}
	asked = nil

	if _, diags := newEngine(publishedData(), nil, &asked).LoadStack(t.Context(), files, "s"); diags.HasErrors() {
		t.Fatal(diags)
	}

	if len(asked) != 1 || asked[0].Digest == "" || !strings.Contains(string(lock), asked[0].Digest) {
		t.Errorf("requests = %+v, want the digest of the lock", asked)
	}

	keys := hostKeys(t)
	want := rigger.ErrNotTrusted.Error()
	e := newEngine(publishedData(), keys, &asked)

	if _, diags := e.LoadStack(t.Context(), files, "s"); !strings.Contains(diags.Error(), want) {
		t.Errorf("a part: %v, want %s", diags, want)
	}

	if _, diags := e.FetchStack(t.Context(), "github.com/acme/rigger//stacks/data", ""); !strings.Contains(diags.Error(), want) {
		t.Errorf("a published stack: %v, want %s", diags, want)
	}

	tree := fstest.MapFS{"modules/web/metadata.hcl": {Data: []byte("module {\n  name    = \"web\"\n  version = \"1.0.0\"\n}\n")}}
	req := source.Request{Source: "acme/web", Kind: source.KindGit, Path: "modules/web", TagPrefix: "web/v", Version: publishedVersion}

	if _, diags := newEngine(tree, keys, &asked).FetchModule(t.Context(), req, nil); !strings.Contains(diags.Error(), want) {
		t.Errorf("a module: %v, want %s", diags, want)
	}
}

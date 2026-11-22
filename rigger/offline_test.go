// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"context"
	"crypto/ecdsa"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/sign"
	"github.com/bacanoo-io/norca/rigger/source"
)

// The repository of an author has its own layout: the index says the path.
const (
	authorStack  = "published/data"
	authorModule = "mods/web"
)

// globalSource is the global registry with its index, and without: then a
// cache or a bundle has the tree, at the path the lock file records.
type globalSource struct {
	tree     fstest.MapFS
	listings map[string]*source.Listing
	offline  bool
}

func (g globalSource) Versions(context.Context, source.Request) ([]source.Available, error) {
	return []source.Available{{Version: publishedVersion, Ref: "v" + publishedVersion, Commit: partCommit}}, nil
}

func (g globalSource) Fetch(_ context.Context, req source.Request) (source.Fetched, error) {
	out := source.Fetched{
		FS: g.tree, Dir: req.Path,
		Origin: source.Origin{
			Source: req.Source, Version: req.Version, Ref: "v" + publishedVersion, Commit: partCommit,
			Repository: "github.com/author/modules",
		},
	}

	if g.offline {
		return out, nil
	}

	out.Dir, out.Listing = authorModule, g.listings[req.Name]
	if req.Stack {
		out.Dir = authorStack
	}

	return out, nil
}

// namespaceKey is a key of a namespace, with its public key and its ID.
func namespaceKey(t *testing.T) (*ecdsa.PrivateKey, []byte, string) {
	t.Helper()

	key, err := sign.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	pub, err := sign.MarshalPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	keyID, err := sign.KeyID(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	return key, pub, keyID
}

// Without the index there are no keys of a namespace to check the signers
// of a lock by: the lock's digest and the trust of the stack hold, so a
// module and a part of the global registry install from a bundle. A signer
// whose key the stack itself has must still sign.
func TestLoadStack_GlobalWithoutIndex(t *testing.T) {
	t.Parallel()

	key, pub, keyID := namespaceKey(t)

	tree := fstest.MapFS{
		authorStack + "/" + metadataFile:  {Data: []byte("stack {\n  name    = \"data\"\n  version = \"1.0.0\"\n}\n")},
		authorStack + "/stack.hcl":        {Data: []byte("rigger {\n  name = \"data\"\n}\n")},
		authorModule + "/" + metadataFile: {Data: []byte("module {\n  name    = \"web\"\n  version = \"1.0.0\"\n}\n")},
	}

	newEngine := func(fetcher source.Fetcher) *rigger.Engine {
		e, err := rigger.New(rigger.WithLogger(slog.New(slog.DiscardHandler)), rigger.WithFetcher(fetcher))
		if err != nil {
			t.Fatal(err)
		}

		return e
	}

	plain := newEngine(globalSource{tree: tree})

	moduleDigest, err := plain.ModuleDigest(t.Context(), tree, authorModule)
	if err != nil {
		t.Fatal(err)
	}

	read, diags := plain.ReadStack(t.Context(), tree, authorStack)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	stackDigest, err := read.Digest()
	if err != nil {
		t.Fatal(err)
	}

	listings := map[string]*source.Listing{}

	for name, digest := range map[string]string{"web": moduleDigest, "data": stackDigest} {
		sig, err := sign.Sign(key, digest, name, publishedVersion)
		if err != nil {
			t.Fatal(err)
		}

		listings[name] = &source.Listing{
			Name: name, Digest: digest, Keys: [][]byte{pub},
			Signatures: []source.ListedSignature{{KeyID: keyID, Signature: sig.Signature}},
		}
	}

	const body = "rigger {\n  name = \"site\"\n%TRUST%}\n\nstack \"data\" {\n  source = \"author/data\"\n}\n\nmodule \"web\" {\n  source = \"author/web\"\n}\n"

	files := stackFiles(strings.Replace(body, "%TRUST%", "", 1))

	_, lock, diags := newEngine(globalSource{tree: tree, listings: listings}).SyncStack(t.Context(), files, "s", false)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if strings.Count(string(lock), keyID) != 2 {
		t.Fatalf("the lock doesn't record the signers:\n%s", lock)
	}

	files["s/rigger.lock.hcl"] = &fstest.MapFile{Data: lock}
	offline := newEngine(globalSource{tree: tree, offline: true})

	if _, diags := offline.LoadStack(t.Context(), files, "s"); diags.HasErrors() {
		t.Errorf("without the index: %v", diags)
	}

	trust := "\n  trust {\n    public_keys = [<<EOT\n" + string(pub) + "EOT\n    ]\n  }\n"
	trusted := stackFiles(strings.Replace(body, "%TRUST%", trust, 1))
	trusted["s/rigger.lock.hcl"] = &fstest.MapFile{Data: lock}

	if _, diags := offline.LoadStack(t.Context(), trusted, "s"); !strings.Contains(diags.Error(), "no longer verifies") {
		t.Errorf("a signer the stack knows that doesn't sign: %v", diags)
	}
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bacanoo-io/norca/rigger/host"
	"github.com/bacanoo-io/norca/rigger/sign"
	"github.com/bacanoo-io/norca/rigger/source"
)

// The module of the tests and its source.
const (
	webName   = "web"
	webSource = "acme/web"
	webTag    = "web/v1.0.0"
)

// webModule is the module web 1.0.0 at modules/web of a repository.
var webModule = fstest.MapFS{
	"modules/web/metadata.hcl":  {Data: []byte("module {\n  name    = \"web\"\n  version = \"1.0.0\"\n}\n")},
	"modules/web/variables.hcl": {Data: []byte("variable \"count\" {\n  type    = number\n  default = 1\n}\n")},
}

// moduleFetcher fetches webModule, with listing as what an index lists of
// it (nil for a source without an index).
type moduleFetcher struct{ listing *source.Listing }

func (moduleFetcher) Versions(context.Context, source.Request) ([]source.Available, error) {
	return nil, nil
}

func (f moduleFetcher) Fetch(_ context.Context, req source.Request) (source.Fetched, error) {
	return source.Fetched{
		FS: webModule, Dir: "modules/web", Listing: f.listing,
		Origin: source.Origin{Source: req.Source, Version: req.Version, Ref: webTag, Commit: strings.Repeat("a", 40)},
	}, nil
}

// webDigest is the tree digest of webModule.
func webDigest(t *testing.T) string {
	t.Helper()

	e, err := New()
	if err != nil {
		t.Fatal(err)
	}

	m, diags := e.LoadModuleAt(t.Context(), webModule, "modules/web")
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	digest, err := m.Digest()
	if err != nil {
		t.Fatal(err)
	}

	return digest
}

func newKey(t *testing.T) (*ecdsa.PrivateKey, string, string) {
	t.Helper()

	key, err := sign.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	pub, err := sign.MarshalPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	id, err := sign.KeyID(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	return key, string(pub), id
}

// FetchModule takes a version as an install on a host takes it: one an
// index lists only with its digest and a signature of a key of its
// namespace; with trusted keys only signed by one of them; one nothing
// signs with a warning. The release of its install records its source, tag
// and signers.
func TestEngine_FetchModule(t *testing.T) {
	t.Parallel()

	digest := webDigest(t)
	nsKey, nsPub, nsID := newKey(t)
	_, otherPub, _ := newKey(t)

	sig, err := sign.Sign(nsKey, digest, webName, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}

	listed := func(d string) *source.Listing {
		return &source.Listing{
			Name: webName, Digest: d, Keys: [][]byte{[]byte(nsPub)},
			Signatures: []source.ListedSignature{{KeyID: nsID, Signature: sig.Signature}},
			Warnings:   []string{"an advisory"},
		}
	}

	req := source.Request{Source: webSource, Kind: source.KindGlobal, Namespace: "acme", Name: webName, Version: "1.0.0"}

	tests := []struct {
		name     string
		listing  *source.Listing
		keys     []string
		req      source.Request
		signers  []string
		warnings string
		err      string
	}{
		{name: "listed and signed", listing: listed(digest), req: req, signers: []string{nsID}, warnings: "an advisory"},
		{name: "listed with another digest", listing: listed("sha256:" + strings.Repeat("0", 64)), req: req, err: "is not what the index lists"},
		{name: "unsigned, no keys", req: req, warnings: "is not signed"},
		{name: "unsigned, keys required", req: req, keys: []string{otherPub}, err: ErrNotTrusted.Error()},
		{name: "no exact version", req: source.Request{Source: webSource, Kind: source.KindGlobal, Name: webName}, err: "an exact version"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e, err := New(WithFetcher(moduleFetcher{listing: tt.listing}), WithReleases(fakeReleases{}), WithCluster(fakeCluster{}))
			if err != nil {
				t.Fatal(err)
			}

			m, diags := e.FetchModule(t.Context(), tt.req, tt.keys)
			if tt.err != "" {
				if !strings.Contains(diags.Error(), tt.err) {
					t.Fatalf("FetchModule() = %v, want %q", diags, tt.err)
				}

				return
			}

			if diags.HasErrors() {
				t.Fatal(diags)
			}

			checkFetched(t, e, m, digest, tt.signers, tt.warnings)
		})
	}
}

// checkFetched checks the origin of a fetched module and what the release of
// its install records.
func checkFetched(t *testing.T, e *Engine, m *Module, digest string, signers []string, warnings string) {
	t.Helper()

	o := m.Origin()
	if o.Digest != digest || o.Ref != webTag || strings.Join(o.Signers, ",") != strings.Join(signers, ",") ||
		!strings.Contains(strings.Join(o.Warnings, "; "), warnings) {
		t.Errorf("origin %+v, want the digest, the tag, signers %v and warnings %q", o, signers, warnings)
	}

	rp, diags := e.PlanInstall(t.Context(), m, Install{Release: webName, Namespace: "apps", Region: "global"}, host.Settings{})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	got := rp.Module
	if got.Source != webSource || got.Ref != webTag || got.Digest != digest || len(got.Signers) != len(signers) {
		t.Errorf("the release records %+v", got)
	}
}

// A module loaded from files records the source of the install, "local" on
// a server, and the module of the global registry is not a module of files.
func TestEngine_PlanInstallLocalSource(t *testing.T) {
	t.Parallel()

	e, err := New(WithReleases(fakeReleases{}), WithCluster(fakeCluster{}))
	if err != nil {
		t.Fatal(err)
	}

	m, diags := e.LoadModuleAt(t.Context(), webModule, "modules/web")
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if m.Origin() != nil {
		t.Error("a module of files has an origin")
	}

	for _, tt := range []struct{ source, want string }{{"local", "local"}, {"", "modules/web"}} {
		rp, diags := e.PlanInstall(t.Context(), m, Install{Release: "web", Namespace: "apps", Region: "global", Source: tt.source}, host.Settings{})
		if diags.HasErrors() {
			t.Fatal(diags)
		}

		if got := rp.Module.Source; got != tt.want {
			t.Errorf("Install.Source %q: the release records %q, want %q", tt.source, got, tt.want)
		}
	}

	if _, diags := e.FetchModule(t.Context(), source.Request{Source: "acme/web", Version: "1.0.0"}, nil); !errors.Is(diagsErr(diags), ErrNoFetcher) && !strings.Contains(diags.Error(), "fetcher") {
		t.Errorf("FetchModule() without a fetcher = %v", diags)
	}
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/rigger/host"
	"github.com/bacanoo-io/norca/rigger/sign"
	"github.com/bacanoo-io/norca/rigger/source"
)

// The module of the tests and its source.
const (
	webName    = "web"
	webSource  = "acme/web"
	webTag     = "web/v1.0.0"
	webDir     = "modules/web"
	webBuiltin = "builtin/web"
	webV1      = "1.0.0"
	webV2      = "2.0.0"
	webTag2    = "web/v2.0.0"
	anAdvisory = "an advisory"
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
		FS: webModule, Dir: webDir, Listing: f.listing,
		Origin: source.Origin{Source: req.Source, Version: req.Version, Ref: webTag, Commit: strings.Repeat("a", 40)},
	}, nil
}

// webDigest is the tree digest of webModule.
func webDigest(t *testing.T) string {
	t.Helper()

	return digestAt(t, webModule)
}

// digestAt is the tree digest of the module at modules/web of fsys.
func digestAt(t *testing.T, fsys fs.FS) string {
	t.Helper()

	e, err := New()
	if err != nil {
		t.Fatal(err)
	}

	m, diags := e.LoadModuleAt(t.Context(), fsys, webDir)
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

	sig, err := sign.Sign(nsKey, digest, webName, webV1)
	if err != nil {
		t.Fatal(err)
	}

	listed := func(d string) *source.Listing {
		return &source.Listing{
			Name: webName, Digest: d, Keys: [][]byte{[]byte(nsPub)},
			Signatures: []source.ListedSignature{{KeyID: nsID, Signature: sig.Signature}},
			Warnings:   []string{anAdvisory},
		}
	}

	req := source.Request{Source: webSource, Kind: source.KindGlobal, Namespace: "acme", Name: webName, Version: webV1}

	tests := []struct {
		name     string
		listing  *source.Listing
		keys     []string
		req      source.Request
		signers  []string
		warnings string
		err      string
	}{
		{name: "listed and signed", listing: listed(digest), req: req, signers: []string{nsID}, warnings: anAdvisory},
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

	if o.Commit == "" || got.Commit != o.Commit {
		t.Errorf("the release records the commit %q, want the commit the module was fetched at, %q", got.Commit, o.Commit)
	}
}

// webAt is the module web of a version at modules/web of a repository.
func webAt(v string) fstest.MapFS {
	return fstest.MapFS{
		"modules/web/metadata.hcl":  {Data: fmt.Appendf(nil, "module {\n  name    = \"web\"\n  version = %q\n}\n", v)},
		"modules/web/variables.hcl": webModule["modules/web/variables.hcl"],
	}
}

// newestFetcher has the versions of the module web: those of versions for
// a source with versions, each at a commit of its own and, in listings, as
// an index lists it; one version, 1.0.0, for a built-in module, as the
// fetcher of the program has it (no version, no tag).
type newestFetcher struct {
	versions []string
	listings map[string]*source.Listing
}

func (f newestFetcher) Versions(_ context.Context, req source.Request) ([]source.Available, error) {
	if req.Kind == source.KindBuiltin {
		return []source.Available{{}}, nil
	}

	out := make([]source.Available, 0, len(f.versions))
	for _, v := range f.versions {
		out = append(out, source.Available{Version: v, Ref: "web/v" + v, Commit: webCommit(v)})
	}

	return out, nil
}

func (f newestFetcher) Fetch(_ context.Context, req source.Request) (source.Fetched, error) {
	if req.Kind == source.KindBuiltin {
		return source.Fetched{FS: webModule, Dir: webDir, Origin: source.Origin{Source: req.Source}}, nil
	}

	return source.Fetched{
		FS: webAt(req.Version), Dir: webDir, Listing: f.listings[req.Version],
		Origin: source.Origin{Source: req.Source, Version: req.Version, Ref: req.Ref, Commit: req.Commit},
	}, nil
}

// webCommit is the commit of a version of newestFetcher.
func webCommit(v string) string {
	return fmt.Sprintf("%040x", len(v)) + v
}

// FetchNewestModule takes the newest version of a source that meets a
// constraint, verified as FetchModule verifies it: of the global registry
// only listed and signed, with its advisories as warnings; of git unsigned
// with a warning; a built-in module of its one version, without
// signatures. The release of its install records where it came from. A
// directory is not fetched.
func TestEngine_FetchNewestModule(t *testing.T) {
	t.Parallel()

	nsKey, nsPub, nsID := newKey(t)

	listings := map[string]*source.Listing{}

	for _, v := range []string{webV1, "1.5.0", webV2} {
		digest := digestAt(t, webAt(v))

		sig, err := sign.Sign(nsKey, digest, webName, v)
		if err != nil {
			t.Fatal(err)
		}

		listings[v] = &source.Listing{
			Name: webName, Digest: digest, Keys: [][]byte{[]byte(nsPub)},
			Signatures: []source.ListedSignature{{KeyID: nsID, Signature: sig.Signature}},
		}
	}

	advised := map[string]*source.Listing{}

	for v, l := range listings {
		c := *l
		c.Warnings = []string{anAdvisory}
		advised[v] = &c
	}

	versions := []string{webV1, webV2, "1.5.0"}

	tests := []struct {
		name, address, constraint string
		listings                  map[string]*source.Listing
		version, ref              string
		signers                   []string
		// warning is a warning the diagnostics have; empty for none.
		warning string
		err     string
	}{
		{name: "newest of several", address: webSource, listings: listings, version: webV2, ref: webTag2, signers: []string{nsID}},
		{name: "a constraint", address: webSource, constraint: "~> 1.0", listings: listings, version: "1.5.0", ref: "web/v1.5.0", signers: []string{nsID}},
		{name: "no version meets", address: webSource, constraint: ">= 3.0.0", listings: listings, err: "no version meets"},
		{name: "an advisory", address: webSource, listings: advised, version: webV2, ref: webTag2, signers: []string{nsID}, warning: "acme/web 2.0.0: an advisory"},
		{name: "git, unsigned", address: "example.com/acme/modules//web", version: webV2, ref: webTag2, warning: "is not signed"},
		{name: "builtin meets", address: webBuiltin, constraint: ">= 1.0.0", version: webV1},
		{name: "builtin, no constraint", address: webBuiltin, version: webV1},
		{name: "builtin does not meet", address: webBuiltin, constraint: ">= 2.0.0", err: `builtin/web is version 1.0.0 in this program: no version meets the constraint ">= 2.0.0"`},
		{name: "a directory", address: "dir://modules/web", err: ErrDirectorySource.Error()},
		{name: "no address", address: "not an address", err: "Invalid module source"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e, err := New(WithFetcher(newestFetcher{versions: versions, listings: tt.listings}), WithReleases(fakeReleases{}), WithCluster(fakeCluster{}))
			if err != nil {
				t.Fatal(err)
			}

			m, diags := e.FetchNewestModule(t.Context(), tt.address, tt.constraint)
			if tt.err != "" {
				if !diags.HasErrors() || !strings.Contains(diags.Error(), tt.err) {
					t.Fatalf("FetchNewestModule() = %v, want %q", diags, tt.err)
				}

				return
			}

			if diags.HasErrors() {
				t.Fatal(diags)
			}

			checkWarning(t, diags, tt.warning)
			checkNewest(t, e, m, tt.address, tt.version, tt.ref, tt.signers)
		})
	}

	bare, err := New()
	if err != nil {
		t.Fatal(err)
	}

	if _, diags := bare.FetchNewestModule(t.Context(), webSource, ""); !strings.Contains(diags.Error(), ErrNoFetcher.Error()) {
		t.Errorf("FetchNewestModule() without a fetcher = %v", diags)
	}
}

// checkWarning checks that diags has a warning with want in it, or none
// when want is empty.
func checkWarning(t *testing.T, diags hcl.Diagnostics, want string) {
	t.Helper()

	var warnings []string

	for _, d := range diags {
		if d.Severity == hcl.DiagWarning {
			warnings = append(warnings, d.Summary+": "+d.Detail)
		}
	}

	got := strings.Join(warnings, "; ")
	if (want == "" && got != "") || !strings.Contains(got, want) {
		t.Errorf("warnings %q, want %q", got, want)
	}
}

// checkNewest checks the origin of a module FetchNewestModule fetched and
// what the release of its install records.
func checkNewest(t *testing.T, e *Engine, m *Module, address, version, ref string, signers []string) {
	t.Helper()

	// What a fetch records: a version of a source with versions at the
	// tag and commit of the version.
	type fetchedAs struct{ source, version, ref, commit, signers string }

	want := fetchedAs{source: address, version: version, ref: ref, signers: strings.Join(signers, ",")}
	if ref != "" {
		want.commit = webCommit(version)
	}

	o := m.Origin()
	if o == nil {
		t.Fatal("a fetched module has no origin")
	}

	if got := (fetchedAs{o.Source, o.Version, o.Ref, o.Commit, strings.Join(o.Signers, ",")}); got != want {
		t.Errorf("origin %+v, want %+v", got, want)
	}

	rp, diags := e.PlanInstall(t.Context(), m, Install{Release: webName, Namespace: "apps", Region: "global"}, host.Settings{})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	r := rp.Module
	if got := (fetchedAs{r.Source, r.Version, r.Ref, r.Commit, strings.Join(r.Signers, ",")}); got != want || r.Digest != o.Digest {
		t.Errorf("the release records %+v and the digest %s, want %+v and %s", got, r.Digest, want, o.Digest)
	}
}

// The versions of a module by its address, the newest first, are those an
// install chooses among: what is no version is left out, a version listed
// twice is listed once; a built-in module has the one the program ships. A
// directory has none to list.
func TestEngine_ModuleVersions(t *testing.T) {
	t.Parallel()

	e, err := New(WithFetcher(newestFetcher{versions: []string{webV1, webV2, "1.4.0", "not-a-version", webV1}}))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, address string
		expected      string
		err           string
	}{
		{name: "the global registry", address: webSource, expected: "2.0.0 1.4.0 1.0.0"},
		{name: "git", address: "example.com/acme/modules//web", expected: "2.0.0 1.4.0 1.0.0"},
		{name: "builtin", address: webBuiltin, expected: webV1},
		{name: "a directory", address: "dir://modules/web", err: ErrDirectorySource.Error()},
		{name: "no address", address: "not an address", err: source.ErrAddress.Error()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := e.ModuleVersions(t.Context(), tt.address)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("ModuleVersions() = %v, %v, want %q", got, err, tt.err)
				}

				return
			}

			if err != nil || strings.Join(got, " ") != tt.expected {
				t.Errorf("ModuleVersions() = %v, %v, want %s", got, err, tt.expected)
			}
		})
	}

	bare, err := New()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := bare.ModuleVersions(t.Context(), webSource); !errors.Is(err, ErrNoFetcher) {
		t.Errorf("ModuleVersions() without a fetcher = %v", err)
	}
}

// stackFetcher lists versions for the requests of a stack only, as the
// global registry lists a stack apart from a module of its name.
type stackFetcher struct{ newestFetcher }

func (f stackFetcher) Versions(ctx context.Context, req source.Request) ([]source.Available, error) {
	if !req.Stack {
		return nil, fmt.Errorf("no module %s: %w", req.Source, fs.ErrNotExist)
	}

	return f.newestFetcher.Versions(ctx, req)
}

// The versions of a published stack are asked for as a stack's; only the
// global registry and git have published stacks.
func TestEngine_StackVersions(t *testing.T) {
	t.Parallel()

	e, err := New(WithFetcher(stackFetcher{newestFetcher{versions: []string{webV1, webV2}}}))
	if err != nil {
		t.Fatal(err)
	}

	for _, address := range []string{webSource, "example.com/acme/stacks//stacks/web"} {
		if got, err := e.StackVersions(t.Context(), address); err != nil || strings.Join(got, " ") != "2.0.0 1.0.0" {
			t.Errorf("StackVersions(%s) = %v, %v", address, got, err)
		}
	}

	if _, err := e.ModuleVersions(t.Context(), webSource); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ModuleVersions() of a stack = %v, want fs.ErrNotExist", err)
	}

	if _, err := e.StackVersions(t.Context(), webBuiltin); !errors.Is(err, ErrNotStackSource) {
		t.Errorf("StackVersions() of a built-in module = %v, want ErrNotStackSource", err)
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

	m, diags := e.LoadModuleAt(t.Context(), webModule, webDir)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if m.Origin() != nil {
		t.Error("a module of files has an origin")
	}

	for _, tt := range []struct{ source, want string }{{"local", "local"}, {"", webDir}} {
		rp, diags := e.PlanInstall(t.Context(), m, Install{Release: "web", Namespace: "apps", Region: "global", Source: tt.source}, host.Settings{})
		if diags.HasErrors() {
			t.Fatal(diags)
		}

		if got := rp.Module.Source; got != tt.want {
			t.Errorf("Install.Source %q: the release records %q, want %q", tt.source, got, tt.want)
		}
	}

	if _, diags := e.FetchModule(t.Context(), source.Request{Source: "acme/web", Version: webV1}, nil); !errors.Is(diagsErr(diags), ErrNoFetcher) && !strings.Contains(diags.Error(), "fetcher") {
		t.Errorf("FetchModule() without a fetcher = %v", diags)
	}
}

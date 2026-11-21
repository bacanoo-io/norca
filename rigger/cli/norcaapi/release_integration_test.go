// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build integration

// The spike of phase 0 for the release API (design 15, phase 0): resources
// evaluated by Rigger applied through the release API of today, against a
// running agent (NORCA_ADDR, default http://127.0.0.1:4646).
//
//	norca agent -dev &
//	go test -tags integration -run TestReleaseSpike -v ./norcaapi

package norcaapi_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/norca/api"

	"github.com/bacanoo-io/norca/rigger/cli/norcaapi"
)

const (
	// moduleDigest stands for the tree digest of a module version.
	moduleDigest = "sha256:5e7a9c1b3d5f7e9a1c3b5d7f9e1a3c5b7d9f1e3a1c3e5a7b9d1f3e5c7a9b1d3f"
	// releaseDigest stands for Rigger's digest of the module digest, the
	// values and the settings of a release (design 9.2).
	releaseDigest = "sha256:0d1f3e5a7c9b1d3f5e7a9c1b3d5f7e9a1c3b5d7f9e1a3c5b7d9f1e3a5c7b9d1f"
	// owner is how the spike marks the stack that owns the release.
	owner = "rigger stack platform"
	// release and module name the release and its module.
	release = "web"
	module  = "web"
)

func client(t *testing.T) *api.Client {
	t.Helper()

	cfg := api.DefaultConfig()
	if addr := os.Getenv("NORCA_ADDR"); addr != "" {
		cfg.Address = addr
	}

	c, err := api.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}

	return c
}

// request is what the rigger program would send for the module web of the
// stack platform: the module in the pack fields, Rigger's digest in
// ValuesHash, the owner in the description, and the evaluated resources.
func request(t *testing.T, namespace string, sigs []*api.ReleasePackSignature) *api.ReleaseApplyRequest {
	t.Helper()

	job, diags := norcaapi.JobDecoder{}.DecodeJob(t.Context(), spec(t, webJob))
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	var apiJob api.Job
	if err := json.Unmarshal(job.Spec, &apiJob); err != nil {
		t.Fatal(err)
	}

	return &api.ReleaseApplyRequest{
		Pack: &api.ReleasePack{
			Name:       module,
			Version:    "0.1.0",
			Source:     "github.com/example/rigger-registry//modules/web",
			Ref:        "web/v0.1.0",
			Digest:     moduleDigest,
			Signatures: sigs,
			Requires:   &api.ReleasePackRequires{Features: []string{}},
		},
		Description: owner,
		Values:      map[string]any{"count": 2},
		ValuesHash:  releaseDigest,
		Resources: []*api.ReleaseResource{
			{
				Type: api.ReleaseResourceVariable,
				Variable: &api.Variable{
					Namespace: namespace,
					Path:      "rigger/spike/web",
					Items:     map[string]string{"release": "web"},
				},
			},
			{Type: api.ReleaseResourceJob, Job: &apiJob},
		},
	}
}

func TestReleaseSpike_ApplyWithoutPackCatalog(t *testing.T) {
	t.Parallel()

	c := client(t)
	w := &api.WriteOptions{Namespace: "default"}
	q := &api.QueryOptions{Namespace: "default"}

	resp, _, err := c.Releases().Apply(release, request(t, "default", nil), w)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	t.Logf("apply: revision %d, warnings %v", resp.Revision, resp.Warnings)

	for _, ch := range resp.Changes {
		t.Logf("  change %+v", *ch)
	}

	rev, _, err := c.Releases().Revision(release, resp.Revision, q)
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}

	spec := rev.Spec
	t.Logf("revision: pack %s %s digest %s, valuesHash %s, description %q, contentDigest %s, values %v",
		spec.Pack.Name, spec.Pack.Version, spec.Pack.Digest, spec.ValuesHash, spec.Description, spec.ContentDigest, spec.Values)

	if spec.Pack.Digest != moduleDigest || spec.ValuesHash != releaseDigest || spec.Description != owner {
		t.Errorf("the revision does not keep what Rigger needs: %+v", spec)
	}

	plan, _, err := c.Releases().Plan(release, request(t, "default", nil), w)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	for _, ch := range plan.Changes {
		t.Logf("  plan of the same request: %+v", *ch)
	}

	again, _, err := c.Releases().Apply(release, request(t, "default", nil), w)
	if err != nil {
		t.Fatalf("Apply again: %v", err)
	}

	t.Logf("apply of the same request again: revision %d", again.Revision)

	releases, _, err := c.Releases().List(q)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	for _, r := range releases {
		t.Logf("list: %s/%s description %q pack %+v", r.Namespace, r.Name, r.Spec.Description, r.Spec.Pack)
	}
}

// sign makes a signature in the format the admission of today verifies:
// a cosign simple signing payload naming the digest, here with the module
// and version of Rigger in its optional part.
func sign(t *testing.T, key *ecdsa.PrivateKey, digest string) *api.ReleasePackSignature {
	t.Helper()

	payload, err := json.Marshal(map[string]any{
		"critical": map[string]any{
			"identity": map[string]any{"docker-reference": "github.com/example/rigger-registry//modules/web"},
			"image":    map[string]any{"docker-manifest-digest": digest},
			"type":     "cosign container image signature",
		},
		"optional": map[string]any{"rigger.module": module, "rigger.version": "0.1.0"},
	})
	if err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256(payload)

	sig, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}

	return &api.ReleasePackSignature{Payload: string(payload), Signature: base64.StdEncoding.EncodeToString(sig)}
}

func TestReleaseSpike_TrustedKeys(t *testing.T) {
	t.Parallel()

	const ns = "rigger-signed"

	c := client(t)

	if _, err := c.Namespaces().Register(&api.Namespace{Name: ns}, nil); err != nil {
		t.Fatalf("namespace: %v", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256(der)
	keyID := "sha256:" + hex.EncodeToString(sum[:])

	policy := &api.AdmissionPolicy{
		Name:        "rigger-spike-trust",
		Action:      "deny",
		Match:       &api.AdmissionPolicyMatch{Namespaces: []string{ns}, Resources: []string{"release"}},
		TrustedKeys: []string{string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))},
	}
	if _, err := c.AdmissionPolicies().Register(policy, nil); err != nil {
		t.Fatalf("admission policy: %v", err)
	}

	w := &api.WriteOptions{Namespace: ns}

	_, _, err = c.Releases().Apply(release, request(t, ns, nil), w)
	if err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Errorf("unsigned apply: error = %v, want a denial", err)
	}

	t.Logf("unsigned: %v", err)

	resp, _, err := c.Releases().Apply(release, request(t, ns, []*api.ReleasePackSignature{sign(t, key, moduleDigest)}), w)
	if err != nil {
		t.Fatalf("signed apply: %v", err)
	}

	rev, _, err := c.Releases().Revision(release, resp.Revision, &api.QueryOptions{Namespace: ns})
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("signed: trusted signer %s (key %s)", rev.Spec.Pack.TrustedSigner, keyID)

	if rev.Spec.Pack.TrustedSigner != keyID {
		t.Errorf("trusted signer = %q, want %q", rev.Spec.Pack.TrustedSigner, keyID)
	}
}

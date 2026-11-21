// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build integration

package command

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bacanoo-io/norca/norca/api"
)

// TestIntegration_TrustedKeys installs a module signed with rigger key sign
// into a namespace whose admission policy trusts the key: the server
// verifies the signatures Rigger sends and records the signer (design 8.2).
func TestIntegration_TrustedKeys(t *testing.T) {
	const ns = "rigger-trust"

	client, err := api.NewClient(api.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	keys := t.TempDir()
	t.Setenv(passwordEnv, "")
	riggerRun(t, exitOK, []string{wroteLock}, "key", "generate", "-out", filepath.Join(keys, "k"))

	pub, err := os.ReadFile(filepath.Join(keys, "k.pub"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.Namespaces().Register(&api.Namespace{Name: ns}, nil); err != nil {
		t.Fatal(err)
	}

	policy := &api.AdmissionPolicy{
		Name: "rigger-trust", Action: "deny",
		Match:       &api.AdmissionPolicyMatch{Namespaces: []string{ns}, Resources: []string{"release"}},
		TrustedKeys: []string{string(pub)},
	}
	if _, err := client.AdmissionPolicies().Register(policy, nil); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_, _, _ = client.Releases().Delete("signed", &api.ReleaseDeleteOptions{Propagation: api.ReleasePropagationBackground}, &api.WriteOptions{Namespace: ns})
		_, _ = client.AdmissionPolicies().Delete("rigger-trust", nil)
	})

	dir := writeTree(t, map[string]string{
		"m/metadata.hcl": "module {\n  name    = \"note\"\n  version = \"1.0.0\"\n}\n",
		"m/note.var.hcl": "variable \"note\" {\n  path = \"riggertrust/note\"\n\n  items {\n    text = \"x\"\n  }\n}\n",
	})
	module := filepath.Join(dir, "m")

	riggerRunErr(t, "not signed", "install", module, "-name", "signed", "-namespace", ns)

	riggerRun(t, exitOK, []string{"signed note 1.0.0"}, "key", "sign", module, "-key", filepath.Join(keys, "k.key"))
	riggerRun(t, exitOK, []string{"install " + ns + "/signed: revision"}, "install", module, "-name", "signed", "-namespace", ns)

	rel, _, err := client.Releases().Info("signed", &api.QueryOptions{Namespace: ns})
	if err != nil {
		t.Fatal(err)
	}

	if rel.Spec.Pack == nil || rel.Spec.Pack.TrustedSigner == "" {
		t.Errorf("the release has no trusted signer: %+v", rel.Spec.Pack)
	}
}

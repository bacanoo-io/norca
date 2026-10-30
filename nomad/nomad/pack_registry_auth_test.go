// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

func TestPacks_AuthSecret(t *testing.T) {
	ci.Parallel(t)
	s, root, cleanup := TestACLServer(t, nil)
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		key, err := s.State().GetActiveRootKeyMeta(nil)
		return err == nil && key != nil
	}), wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))
	codec := rpcClient(t, s)

	putVar := func(path string, items map[string]string) {
		req := &structs.VariablesApplyRequest{
			Op:           structs.VarOpSet,
			Var:          &structs.VariableDecrypted{VariableMetadata: structs.VariableMetadata{Namespace: "default", Path: path}, Items: items},
			WriteRequest: structs.WriteRequest{Region: "global", AuthToken: root.SecretID},
		}
		must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.VariablesApplyRPCMethod, req, &structs.VariablesApplyResponse{}))
	}
	putVar("registries/ghcr", map[string]string{"username": "bot", "password": "s3cret"})
	putVar("registries/ssh", map[string]string{"ssh_private_key": "KEY"})

	registryWriter := mock.CreatePolicyAndToken(t, s.State(), 1001, "registry-writer", `
namespace "*" {
  object "packs.norca.io/Registry" {
    capabilities = ["read", "write"]
  }
}`)
	secretReader := mock.CreatePolicyAndToken(t, s.State(), 1002, "registry-admin", `
namespace "*" {
  object "packs.norca.io/Registry" {
    capabilities = ["read", "write"]
  }
}
namespace "default" {
  variables {
    path "registries/*" {
      capabilities = ["read"]
    }
  }
}`)

	upsert := func(token string, spec *PackRegistrySpec) (*PackRegistry, error) {
		var resp PackRegistryResponse
		err := msgpackrpc.CallWithCodec(codec, "Packs.Upsert", &PackRegistryUpsertRequest{
			Registry:     &PackRegistry{Name: "private", Spec: spec},
			WriteRequest: structs.WriteRequest{Region: "global", AuthToken: token},
		}, &resp)
		return resp.Registry, err
	}
	ghcr := &PackRegistryAuthSecret{Path: "registries/ghcr"}

	// Writing registries is not enough to use someone else's credentials.
	_, err := upsert(registryWriter.SecretID, &PackRegistrySpec{Type: "oci", Source: "oci://127.0.0.1:1/packs/app", AuthSecret: ghcr})
	must.ErrorContains(t, err, `can't read the Variable "registries/ghcr"`)

	// With read access the registry is saved; the unreachable source makes
	// the sync fail, but never with the credentials in the status.
	reg, err := upsert(secretReader.SecretID, &PackRegistrySpec{Type: "oci", Source: "oci://127.0.0.1:1/packs/app", AuthSecret: ghcr})
	must.NoError(t, err)
	must.Eq(t, "default", reg.Spec.AuthSecret.Namespace)
	must.Eq(t, PackRegistryPhaseError, reg.Status.Phase)
	must.StrNotContains(t, reg.Status.Message, "s3cret")

	// Changing the source of the registry needs the credentials again.
	_, err = upsert(registryWriter.SecretID, &PackRegistrySpec{Type: "oci", Source: "oci://attacker.example/packs/app", AuthSecret: ghcr})
	must.ErrorContains(t, err, "can't read the Variable")

	// A sync by a registry writer keeps the source the credentials were
	// approved for.
	var synced PackRegistryResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Packs.Sync", &PackRegistryNameRequest{
		Name: "private", WriteRequest: structs.WriteRequest{Region: "global", AuthToken: registryWriter.SecretID},
	}, &synced))
	must.Eq(t, "oci://127.0.0.1:1/packs/app", synced.Registry.Spec.Source)

	for name, tc := range map[string]struct {
		spec *PackRegistrySpec
		want string
	}{
		"dir type":       {&PackRegistrySpec{Type: "dir", Source: "builtin", AuthSecret: ghcr}, "takes no authSecret"},
		"no variable":    {&PackRegistrySpec{Type: "oci", Source: "oci://r.example/p", AuthSecret: &PackRegistryAuthSecret{Path: "registries/none"}}, "does not exist"},
		"plain http git": {&PackRegistrySpec{Type: "git", Source: "http://git.example/packs.git", AuthSecret: ghcr}, "https:// or ssh"},
		"ssh no hosts":   {&PackRegistrySpec{Type: "git", Source: "git@git.example:org/packs.git", AuthSecret: &PackRegistryAuthSecret{Path: "registries/ssh"}}, "known_hosts"},
		"no path":        {&PackRegistrySpec{Type: "oci", Source: "oci://r.example/p", AuthSecret: &PackRegistryAuthSecret{}}, "needs the path"},
	} {
		_, err := upsert(root.SecretID, tc.spec)
		must.ErrorContains(t, err, tc.want, must.Sprint(name))
	}
}

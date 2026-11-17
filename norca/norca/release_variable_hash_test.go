// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"
)

// TestRelease_VariableHashIsKeyed checks that the status of a release, which
// every reader of the release gets, does not hold a hash of the items of a
// Variable that can be tried offline (the secret items are never stored in
// the release): the hash is keyed with a root key of the servers. Drift
// detection still tells a changed Variable from one written again unchanged,
// also after the root key was rotated.
func TestRelease_VariableHashIsKeyed(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()

	_, err := c.apply("default", "app", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "app"},
		Resources: []*structs.ReleaseResource{
			{Type: structs.ReleaseResourceVariable, SecretKeys: []string{"password"}, Variable: &structs.VariableDecrypted{
				VariableMetadata: structs.VariableMetadata{Path: "config/app"},
				Items:            map[string]string{"user": "admin"},
			}},
			{Type: structs.ReleaseResourceNamespace, Namespace: &structs.Namespace{Name: "app-ns"}},
		},
		Secrets: []*structs.ReleaseVariableSecret{{Path: "config/app", Items: map[string]string{"password": "s3cret"}}},
	})
	must.NoError(t, err)

	refHash := func(typ string) string {
		for _, ref := range c.get("default", "app").Status.Resources {
			if ref.Type == typ {
				return ref.Hash
			}
		}
		t.Fatalf("the release has no %s", typ)
		return ""
	}

	// What a reader of the release would try: the hash of a guess of the
	// items. "user" is in the revision; the password is the guess.
	guess, err := json.Marshal(map[string]string{"user": "admin", "password": "s3cret"})
	must.NoError(t, err)
	sum := sha256.Sum256(guess)
	hash := refHash(structs.ReleaseResourceVariable)
	must.NotEq(t, hex.EncodeToString(sum[:]), hash, must.Sprint("the release shows the plain hash of the Variable's items"))
	must.StrHasPrefix(t, releaseVariableHashPrefix, hash)
	// The other resources keep their plain hash: they hold no secret.
	must.False(t, strings.HasPrefix(refHash(structs.ReleaseResourceNamespace), releaseVariableHashPrefix))

	setVar := func(items map[string]string) {
		var resp structs.VariablesApplyResponse
		must.NoError(t, msgpackrpc.CallWithCodec(c.codec, structs.VariablesApplyRPCMethod, &structs.VariablesApplyRequest{
			Op: structs.VarOpSet,
			Var: &structs.VariableDecrypted{
				VariableMetadata: structs.VariableMetadata{Namespace: "default", Path: "config/app"},
				Items:            items,
			},
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: c.token},
		}, &resp))
		must.True(t, resp.IsOk())
	}

	must.False(t, c.drift("default", "app").Drifted)

	// Written again unchanged, with a new root key: no drift.
	var rotated structs.KeyringRotateRootKeyResponse
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, "Keyring.Rotate", &structs.KeyringRotateRootKeyRequest{
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: c.token}}, &rotated))
	setVar(map[string]string{"user": "admin", "password": "s3cret"})
	stored, err := c.srv.State().GetVariable(nil, "default", "config/app")
	must.NoError(t, err)
	must.Eq(t, rotated.Key.KeyID, stored.KeyID)
	d := c.drift("default", "app")
	must.False(t, d.Drifted, must.Sprintf("%v", d.Resources))

	// Changed by hand: drift.
	setVar(map[string]string{"user": "admin", "password": "other"})
	d = c.drift("default", "app")
	must.True(t, d.Drifted)
	must.Len(t, 1, d.Resources)
	must.Eq(t, "config/app", d.Resources[0].Name)
	must.Eq(t, "modified", d.Resources[0].Reason)
}

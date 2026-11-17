// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// An engine may name the operator service of another namespace; the leader
// then asks that operator for credentials with its server certificate and
// the role configuration of the engine (review finding SRV-3). Writing such
// an engine needs write-secret-engine in the namespace of the operator too.
func TestSecretEngine_OperatorOfOtherNamespaceACL(t *testing.T) {
	ci.Parallel(t)
	et := newEngineTest(t, nil)
	store := et.s.fsm.State()

	must.NoError(t, store.UpsertNamespaces(900, []*structs.Namespace{{Name: "platform"}}))
	engineWrite := []string{acl.NamespaceCapabilityWriteSecretEngine}
	devAdmin := mock.CreatePolicyAndToken(t, store, 1001, "dev-admin",
		mock.NamespacePolicy(structs.DefaultNamespace, "", engineWrite))
	both := mock.CreatePolicyAndToken(t, store, 1003, "both",
		mock.NamespacePolicy(structs.DefaultNamespace, "", engineWrite)+"\n"+
			mock.NamespacePolicy("platform", "", engineWrite))

	// Over a connection, as a caller sends it.
	upsert := func(token string, operatorNamespace string) error {
		engine := operatorEngine()
		engine.Operator.Namespace = operatorNamespace
		engine.Roles[0].Config = map[string]string{"database": "prod", "grants": "superuser"}
		codec := rpcClient(t, et.s)
		defer codec.Close()
		return msgpackrpc.CallWithCodec(codec, structs.SecretEngineUpsertRPCMethod, &structs.SecretEngineUpsertRequest{
			Engine:       engine,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: token},
		}, new(structs.SecretEngineWriteResponse))
	}

	// The right in the engine's namespace alone does not reach the operator
	// of another one.
	err := upsert(devAdmin.SecretID, "platform")
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	got, err := store.SecretEngineByName(nil, structs.DefaultNamespace, "pg")
	must.NoError(t, err)
	must.Nil(t, got)

	// Its own namespace, named or by default, is covered by it.
	must.NoError(t, upsert(devAdmin.SecretID, ""))
	must.NoError(t, upsert(devAdmin.SecretID, structs.DefaultNamespace))

	// With the right in both namespaces, and with a management token.
	must.NoError(t, upsert(both.SecretID, "platform"))
	got, err = store.SecretEngineByName(nil, structs.DefaultNamespace, "pg")
	must.NoError(t, err)
	must.Eq(t, "platform", got.Operator.Namespace)
	must.NoError(t, upsert(et.root.SecretID, "platform"))

	// An update of such an engine needs it as well.
	err = upsert(devAdmin.SecretID, "platform")
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
}

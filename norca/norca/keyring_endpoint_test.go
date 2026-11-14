// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"net/http"
	"sync"
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// TestKeyringEndpoint_CRUD exercises the basic keyring operations
func TestKeyringEndpoint_CRUD(t *testing.T) {

	ci.Parallel(t)
	srv, rootToken, shutdown := TestACLServer(t, func(c *Config) {
		c.NumSchedulers = 0 // Prevent automatic dequeue
	})
	defer shutdown()
	testutil.WaitForLeader(t, srv.RPC)
	codec := rpcClient(t, srv)

	// Upsert a new key

	key, err := structs.NewRootKey(structs.EncryptionAlgorithmAES256GCM)
	require.NoError(t, err)
	id := key.Meta.KeyID
	key.Meta.SetActive()

	updateReq := &structs.KeyringUpdateRootKeyRequest{
		RootKey:      key,
		WriteRequest: structs.WriteRequest{Region: "global"},
	}
	var updateResp structs.KeyringUpdateRootKeyResponse

	err = msgpackrpc.CallWithCodec(codec, "Keyring.Update", updateReq, &updateResp)
	require.EqualError(t, err, structs.ErrPermissionDenied.Error())

	updateReq.AuthToken = rootToken.SecretID
	err = msgpackrpc.CallWithCodec(codec, "Keyring.Update", updateReq, &updateResp)
	require.NoError(t, err)
	require.NotEqual(t, uint64(0), updateResp.Index)

	// Get and List don't need a token here because they rely on mTLS role verification
	getReq := &structs.KeyringGetRootKeyRequest{
		KeyID:        id,
		QueryOptions: structs.QueryOptions{Region: "global"},
	}
	var getResp structs.KeyringGetRootKeyResponse

	err = msgpackrpc.CallWithCodec(codec, "Keyring.Get", getReq, &getResp)
	require.NoError(t, err)
	require.Equal(t, updateResp.Index, getResp.Index)
	require.Equal(t, structs.EncryptionAlgorithmAES256GCM, getResp.Key.Meta.Algorithm)

	// Make a blocking query for List and wait for an Update. Note
	// that List/Get queries don't need ACL tokens in the test server
	// because they always pass the mTLS check

	var wg sync.WaitGroup
	wg.Add(1)
	var listResp structs.KeyringListRootKeyMetaResponse

	go func() {
		defer wg.Done()
		codec := rpcClient(t, srv) // not safe to share across goroutines
		listReq := &structs.KeyringListRootKeyMetaRequest{
			QueryOptions: structs.QueryOptions{
				Region:        "global",
				MinQueryIndex: getResp.Index,
			},
		}
		// Its own error: the test goroutine writes err at the same time.
		err := msgpackrpc.CallWithCodec(codec, "Keyring.List", listReq, &listResp)
		assert.NoError(t, err)
	}()

	updateReq.RootKey.Meta.CreateTime = time.Now().UTC().UnixNano()
	err = msgpackrpc.CallWithCodec(codec, "Keyring.Update", updateReq, &updateResp)
	require.NoError(t, err)
	require.NotEqual(t, uint64(0), updateResp.Index)

	// wait for the blocking query to complete and check the response
	wg.Wait()
	require.Equal(t, listResp.Index, updateResp.Index)
	require.Len(t, listResp.Keys, 2) // bootstrap + new one

	// Delete the key and verify that it's gone

	delReq := &structs.KeyringDeleteRootKeyRequest{
		KeyID:        id,
		WriteRequest: structs.WriteRequest{Region: "global"},
	}
	var delResp structs.KeyringDeleteRootKeyResponse

	err = msgpackrpc.CallWithCodec(codec, "Keyring.Delete", delReq, &delResp)
	require.EqualError(t, err, structs.ErrPermissionDenied.Error())

	delReq.AuthToken = rootToken.SecretID
	err = msgpackrpc.CallWithCodec(codec, "Keyring.Delete", delReq, &delResp)
	code, msg, ok := structs.CodeFromRPCCodedErr(err)
	require.True(t, ok, err)
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, "active root key cannot be deleted - call rotate first", msg)

	// set inactive
	updateReq.RootKey.Meta.SetInactive()
	err = msgpackrpc.CallWithCodec(codec, "Keyring.Update", updateReq, &updateResp)
	require.NoError(t, err)

	err = msgpackrpc.CallWithCodec(codec, "Keyring.Delete", delReq, &delResp)
	require.NoError(t, err)
	require.Greater(t, delResp.Index, getResp.Index)

	listReq := &structs.KeyringListRootKeyMetaRequest{
		QueryOptions: structs.QueryOptions{Region: "global"},
	}
	err = msgpackrpc.CallWithCodec(codec, "Keyring.List", listReq, &listResp)
	require.NoError(t, err)
	require.Greater(t, listResp.Index, getResp.Index)
	require.Len(t, listResp.Keys, 1) // just the bootstrap key
}

// TestKeyringEndpoint_validateUpdate exercises all the various
// validations we make for the update RPC
func TestKeyringEndpoint_InvalidUpdates(t *testing.T) {

	ci.Parallel(t)
	srv, rootToken, shutdown := TestACLServer(t, func(c *Config) {
		c.NumSchedulers = 0 // Prevent automatic dequeue
	})
	defer shutdown()
	testutil.WaitForLeader(t, srv.RPC)
	codec := rpcClient(t, srv)

	// Setup an existing key
	key, err := structs.NewRootKey(structs.EncryptionAlgorithmAES256GCM)
	require.NoError(t, err)
	id := key.Meta.KeyID
	key.Meta.SetActive()

	updateReq := &structs.KeyringUpdateRootKeyRequest{
		RootKey: key,
		WriteRequest: structs.WriteRequest{
			Region:    "global",
			AuthToken: rootToken.SecretID,
		},
	}
	var updateResp structs.KeyringUpdateRootKeyResponse
	err = msgpackrpc.CallWithCodec(codec, "Keyring.Update", updateReq, &updateResp)
	require.NoError(t, err)

	testCases := []struct {
		key            *structs.RootKey
		expectedErrMsg string
	}{
		{
			key:            &structs.RootKey{},
			expectedErrMsg: "root key metadata is required",
		},
		{
			key:            &structs.RootKey{Meta: &structs.RootKeyMeta{}},
			expectedErrMsg: "root key UUID is required",
		},
		{
			key:            &structs.RootKey{Meta: &structs.RootKeyMeta{KeyID: "invalid"}},
			expectedErrMsg: "root key UUID is required",
		},
		{
			key: &structs.RootKey{Meta: &structs.RootKeyMeta{
				KeyID:     id,
				Algorithm: structs.EncryptionAlgorithmAES256GCM,
			}},
			expectedErrMsg: "root key state \"\" is invalid",
		},
		{
			key: &structs.RootKey{Meta: &structs.RootKeyMeta{
				KeyID:     id,
				Algorithm: structs.EncryptionAlgorithmAES256GCM,
				State:     structs.RootKeyStateActive,
			}},
			expectedErrMsg: "root key material is required",
		},

		{
			key: &structs.RootKey{
				Key: []byte{0x01},
				Meta: &structs.RootKeyMeta{
					KeyID:     id,
					Algorithm: "whatever",
					State:     structs.RootKeyStateActive,
				}},
			expectedErrMsg: "root key algorithm cannot be changed after a key is created",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.expectedErrMsg, func(t *testing.T) {
			updateReq := &structs.KeyringUpdateRootKeyRequest{
				RootKey: tc.key,
				WriteRequest: structs.WriteRequest{
					Region:    "global",
					AuthToken: rootToken.SecretID,
				},
			}
			var updateResp structs.KeyringUpdateRootKeyResponse
			err := msgpackrpc.CallWithCodec(codec, "Keyring.Update", updateReq, &updateResp)
			require.EqualError(t, err, tc.expectedErrMsg)
		})
	}

}

// TestKeyringEndpoint_Rotate exercises the key rotation logic
func TestKeyringEndpoint_Rotate(t *testing.T) {

	ci.Parallel(t)
	srv, rootToken, shutdown := TestACLServer(t, func(c *Config) {
		c.NumSchedulers = 0 // Prevent automatic dequeue
	})
	defer shutdown()
	testutil.WaitForLeader(t, srv.RPC)
	codec := rpcClient(t, srv)

	// Setup an existing key
	key, err := structs.NewRootKey(structs.EncryptionAlgorithmAES256GCM)
	require.NoError(t, err)
	key.Meta.SetActive()

	updateReq := &structs.KeyringUpdateRootKeyRequest{
		RootKey: key,
		WriteRequest: structs.WriteRequest{
			Region:    "global",
			AuthToken: rootToken.SecretID,
		},
	}
	var updateResp structs.KeyringUpdateRootKeyResponse
	err = msgpackrpc.CallWithCodec(codec, "Keyring.Update", updateReq, &updateResp)
	require.NoError(t, err)

	// Rotate the key

	rotateReq := &structs.KeyringRotateRootKeyRequest{
		WriteRequest: structs.WriteRequest{
			Region: "global",
		},
	}
	var rotateResp structs.KeyringRotateRootKeyResponse
	err = msgpackrpc.CallWithCodec(codec, "Keyring.Rotate", rotateReq, &rotateResp)
	require.EqualError(t, err, structs.ErrPermissionDenied.Error())

	rotateReq.AuthToken = rootToken.SecretID
	err = msgpackrpc.CallWithCodec(codec, "Keyring.Rotate", rotateReq, &rotateResp)
	require.NoError(t, err)
	require.NotEqual(t, updateResp.Index, rotateResp.Index)

	newID := rotateResp.Key.KeyID

	// Verify we have a new key and the old one is inactive

	listReq := &structs.KeyringListRootKeyMetaRequest{
		QueryOptions: structs.QueryOptions{
			Region: "global",
		},
	}
	var listResp structs.KeyringListRootKeyMetaResponse
	err = msgpackrpc.CallWithCodec(codec, "Keyring.List", listReq, &listResp)
	require.NoError(t, err)

	require.Greater(t, listResp.Index, updateResp.Index)
	require.Len(t, listResp.Keys, 3) // bootstrap + old + new

	for _, keyMeta := range listResp.Keys {
		if keyMeta.KeyID != newID {
			require.False(t, keyMeta.Active(), "expected old keys to be inactive")
		} else {
			require.True(t, keyMeta.Active(), "expected new key to be inactive")
		}
	}

	getReq := &structs.KeyringGetRootKeyRequest{
		KeyID: newID,
		QueryOptions: structs.QueryOptions{
			Region: "global",
		},
	}
	var getResp structs.KeyringGetRootKeyResponse
	err = msgpackrpc.CallWithCodec(codec, "Keyring.Get", getReq, &getResp)
	require.NoError(t, err)

	gotKey := getResp.Key
	require.Len(t, gotKey.Key, 32)
}

// TestKeyringEndpoint_DeleteKeyInUse checks that a root key the state still
// needs is not removed by hand: one that encrypts a Variable, until a full
// rotation encrypted the Variable with the new key, and one that signed the
// workload identity of an allocation that has not ended.
func TestKeyringEndpoint_DeleteKeyInUse(t *testing.T) {
	ci.Parallel(t)

	srv, cleanup := TestServer(t, nil)
	defer cleanup()
	testutil.WaitForLeader(t, srv.RPC)
	waitForActiveRootKey(t, srv)
	write := structs.WriteRequest{Region: srv.config.Region}

	// The first key of a server is in use by other things: the test has a
	// key of its own.
	rotate := func(full bool) string {
		var resp structs.KeyringRotateRootKeyResponse
		must.NoError(t, srv.RPC("Keyring.Rotate", &structs.KeyringRotateRootKeyRequest{Full: full, WriteRequest: write}, &resp))
		return resp.Key.KeyID
	}
	oldKey := rotate(false)

	variable := mock.Variable()
	must.NoError(t, srv.RPC("Variables.Apply", &structs.VariablesApplyRequest{
		Op: structs.VarOpSet, Var: variable, WriteRequest: write,
	}, &structs.VariablesApplyResponse{}))

	remove := func() error {
		return srv.RPC("Keyring.Delete", &structs.KeyringDeleteRootKeyRequest{KeyID: oldKey, WriteRequest: write},
			&structs.KeyringDeleteRootKeyResponse{})
	}
	stored := func() *structs.RootKeyMeta {
		meta, err := srv.fsm.State().RootKeyMetaByID(nil, oldKey)
		must.NoError(t, err)
		return meta
	}

	// Not the active key any more, and it encrypts the Variable.
	rotate(false)
	err := remove()
	must.ErrorContains(t, err, "encrypts 1 Variable(s)")
	code, _, ok := structs.CodeFromRPCCodedErr(err)
	must.True(t, ok)
	must.Eq(t, http.StatusConflict, code)
	must.NotNil(t, stored())

	// A full rotation encrypts the Variable with its key: the two older
	// keys encrypt nothing and go.
	newKey := rotate(true)
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		v, err := srv.fsm.State().GetVariable(nil, variable.Namespace, variable.Path)
		return err == nil && v != nil && v.KeyID == newKey
	}), wait.Timeout(20*time.Second), wait.Gap(100*time.Millisecond)))

	// An allocation that has not ended and whose workload identity the key
	// signed keeps it too, until it is gone.
	alloc := mock.Alloc()
	alloc.SigningKeyID = oldKey
	upsert := func() {
		index, err := srv.fsm.State().LatestIndex()
		must.NoError(t, err)
		must.NoError(t, srv.fsm.State().UpsertAllocs(structs.MsgTypeTestSetup, index+1, []*structs.Allocation{alloc}))
	}
	upsert()
	err = remove()
	must.ErrorContains(t, err, "signed the workload identity of 1 allocation(s)")
	code, _, _ = structs.CodeFromRPCCodedErr(err)
	must.Eq(t, http.StatusConflict, code)
	must.NotNil(t, stored())
	index, err := srv.fsm.State().LatestIndex()
	must.NoError(t, err)
	must.NoError(t, srv.fsm.State().DeleteEval(index+1, nil, []string{alloc.ID}, false))

	must.NoError(t, remove())
	must.Nil(t, stored())
}

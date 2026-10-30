// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"io"
	"net"
	"path"
	"testing"
	"time"

	"github.com/hashicorp/go-msgpack/codec"
	"github.com/hashicorp/nomad/ci"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

// TestResolveACL_NilMeansDeny checks the rule that with ACLs enabled a
// request without an ACL token or a workload identity never resolves to a nil
// ACL object (which every capability check treats as "ACLs disabled"), and
// that only ResolveClientOrACL accepts a client node identity.
func TestResolveACL_NilMeansDeny(t *testing.T) {
	ci.Parallel(t)

	s, root, cleanup := TestACLServer(t, nil)
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)

	withIdentity := func(id *structs.AuthenticatedIdentity) *structs.GenericRequest {
		args := &structs.GenericRequest{}
		if id != nil {
			args.SetIdentity(id)
		}
		return args
	}

	denied := map[string]*structs.GenericRequest{
		"no identity":     withIdentity(nil),
		"client node":     withIdentity(&structs.AuthenticatedIdentity{ClientID: uuid.Generate()}),
		"failed auth":     withIdentity(&structs.AuthenticatedIdentity{RemoteIP: net.IPv4(10, 0, 0, 1)}),
		"empty identity":  withIdentity(&structs.AuthenticatedIdentity{}),
		"tls name only":   withIdentity(&structs.AuthenticatedIdentity{TLSName: "server.global.nomad"}),
		"remote ip + tls": withIdentity(&structs.AuthenticatedIdentity{TLSName: "x", RemoteIP: net.IPv4(10, 0, 0, 2)}),
	}
	for name, args := range denied {
		t.Run(name, func(t *testing.T) {
			aclObj, err := s.ResolveACL(args)
			must.EqError(t, err, structs.ErrPermissionDenied.Error())
			must.Nil(t, aclObj)

			aclObj, err = s.ResolveClientOrACL(args)
			if name == "client node" {
				must.NoError(t, err)
				must.Nil(t, aclObj)
			} else {
				must.EqError(t, err, structs.ErrPermissionDenied.Error())
			}
		})
	}

	aclObj, err := s.ResolveACL(withIdentity(&structs.AuthenticatedIdentity{ACLToken: root}))
	must.NoError(t, err)
	must.NotNil(t, aclObj)
	must.True(t, aclObj.IsManagement())

	anonymous, err := s.ResolveACL(withIdentity(&structs.AuthenticatedIdentity{ACLToken: structs.AnonymousACLToken}))
	must.NoError(t, err)
	must.NotNil(t, anonymous)
	must.False(t, anonymous.IsManagement())

	// With ACLs disabled nothing changes: a nil ACL object for everyone.
	s2, cleanup2 := TestServer(t, nil)
	defer cleanup2()
	for name, args := range denied {
		aclObj, err := s2.ResolveACL(args)
		must.NoError(t, err, must.Sprint(name))
		must.Nil(t, aclObj, must.Sprint(name))
		aclObj, err = s2.ResolveClientOrACL(args)
		must.NoError(t, err, must.Sprint(name))
		must.Nil(t, aclObj, must.Sprint(name))
	}
}

// TestNodeSecret_DeniedOnSecretSealKeyring checks that the secret ID of a
// client node, which authenticates as a client identity, is denied on the
// secret store, seal and keyring endpoints and the other endpoints that
// resolve an ACL object, with ACLs enabled (SEC-1), and that nothing changed.
func TestNodeSecret_DeniedOnSecretSealKeyring(t *testing.T) {
	ci.Parallel(t)

	s, root, cleanup := secretTestServer(t, true, nil)
	defer cleanup()
	store := s.fsm.State()

	node := mock.Node()
	must.NoError(t, store.UpsertNode(structs.MsgTypeTestSetup, 1000, node))
	nodeSecret := node.SecretID

	var w structs.SecretWriteResponse
	must.NoError(t, s.RPC(structs.SecretUpsertRPCMethod,
		secretWriteReq("app/db", root.SecretID, structs.SecretItems{"password": "top"}), &w))

	var keys structs.KeyringListRootKeyMetaResponse
	must.NoError(t, s.RPC("Keyring.List", &structs.KeyringListRootKeyMetaRequest{
		QueryOptions: structs.QueryOptions{Region: "global", AuthToken: root.SecretID}}, &keys))
	must.SliceNotEmpty(t, keys.Keys)
	activeKeyID := keys.Keys[0].KeyID

	write := structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: nodeSecret}
	query := structs.QueryOptions{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: nodeSecret}

	calls := map[string]func() error{
		"Secret.Read": func() error {
			var r structs.SecretReadResponse
			return s.RPC(structs.SecretReadRPCMethod, &structs.SecretReadRequest{Path: "app/db", QueryOptions: query}, &r)
		},
		"Secret.ReadMetadata": func() error {
			var r structs.SecretMetadataResponse
			return s.RPC(structs.SecretReadMetadataRPCMethod, &structs.SecretReadMetadataRequest{Path: "app/db", QueryOptions: query}, &r)
		},
		"Secret.List": func() error {
			var r structs.SecretListResponse
			return s.RPC(structs.SecretListRPCMethod, &structs.SecretListRequest{QueryOptions: query}, &r)
		},
		"Secret.Upsert": func() error {
			return s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("app/db", nodeSecret, structs.SecretItems{"password": "evil"}), &w)
		},
		"Secret.Destroy": func() error {
			return s.RPC(structs.SecretDestroyRPCMethod, secretVersionsReq("app/db", nodeSecret), &w)
		},
		"Operator.SealStatus": func() error {
			var r structs.SealStatusResponse
			return s.RPC("Operator.SealStatus", &structs.SealStatusRequest{QueryOptions: query}, &r)
		},
		"Operator.SealUnseal": func() error {
			var r structs.SealUnsealResponse
			return s.RPC("Operator.SealUnseal", &structs.SealUnsealRequest{Reset: true, WriteRequest: write}, &r)
		},
		"Operator.SealRecover": func() error {
			var r structs.SealRecoverResponse
			return s.RPC("Operator.SealRecover", &structs.SealRecoverRequest{Reset: true, WriteRequest: write}, &r)
		},
		"Keyring.Rewrap": func() error {
			var r structs.KeyringRewrapResponse
			return s.RPC("Keyring.Rewrap", &structs.KeyringRewrapRequest{To: structs.SealTypeShares, WriteRequest: write}, &r)
		},
		"Keyring.Rotate": func() error {
			var r structs.KeyringRotateRootKeyResponse
			return s.RPC("Keyring.Rotate", &structs.KeyringRotateRootKeyRequest{WriteRequest: write}, &r)
		},
		"Keyring.Update": func() error {
			key, err := structs.NewRootKey(structs.EncryptionAlgorithmAES256GCM)
			if err != nil {
				return err
			}
			key.Meta.SetActive()
			var r structs.KeyringUpdateRootKeyResponse
			return s.RPC("Keyring.Update", &structs.KeyringUpdateRootKeyRequest{RootKey: key, WriteRequest: write}, &r)
		},
		"Keyring.Delete": func() error {
			var r structs.KeyringDeleteRootKeyResponse
			return s.RPC("Keyring.Delete", &structs.KeyringDeleteRootKeyRequest{KeyID: activeKeyID, WriteRequest: write}, &r)
		},
		"Keyring.Restore": func() error {
			key, err := structs.NewRootKey(structs.EncryptionAlgorithmAES256GCM)
			if err != nil {
				return err
			}
			var r structs.KeyringRestoreResponse
			return s.RPC("Keyring.Restore", &structs.KeyringRestoreRequest{Keys: []*structs.RootKey{key}, WriteRequest: write}, &r)
		},
		"Variables.Read": func() error {
			var r structs.VariablesReadResponse
			return s.RPC(structs.VariablesReadRPCMethod, &structs.VariablesReadRequest{Path: "app/db", QueryOptions: query}, &r)
		},
		"Variables.Apply": func() error {
			var r structs.VariablesApplyResponse
			return s.RPC(structs.VariablesApplyRPCMethod, &structs.VariablesApplyRequest{
				Op: structs.VarOpSet,
				Var: &structs.VariableDecrypted{
					VariableMetadata: structs.VariableMetadata{Namespace: structs.DefaultNamespace, Path: "app/db"},
					Items:            structs.VariableItems{"k": "v"},
				},
				WriteRequest: write,
			}, &r)
		},
		"Alloc.List": func() error {
			var r structs.AllocListResponse
			return s.RPC("Alloc.List", &structs.AllocListRequest{QueryOptions: query}, &r)
		},
		"Backup.List": func() error {
			var r structs.BackupListResponse
			return s.RPC("Backup.List", &structs.BackupListRequest{QueryOptions: query}, &r)
		},
		"Node.UpdateEligibility": func() error {
			var r structs.NodeEligibilityUpdateResponse
			return s.RPC("Node.UpdateEligibility", &structs.NodeUpdateEligibilityRequest{
				NodeID: node.ID, Eligibility: structs.NodeSchedulingIneligible, WriteRequest: write}, &r)
		},
		"Operator.SchedulerGetConfiguration": func() error {
			var r structs.SchedulerConfigurationResponse
			return s.RPC("Operator.SchedulerGetConfiguration", &structs.GenericRequest{QueryOptions: query}, &r)
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			must.EqError(t, call(), structs.ErrPermissionDenied.Error())
		})
	}

	// Nothing changed: the secret keeps its value, the key stays active.
	var r structs.SecretReadResponse
	must.NoError(t, s.RPC(structs.SecretReadRPCMethod, &structs.SecretReadRequest{Path: "app/db",
		QueryOptions: structs.QueryOptions{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: root.SecretID}}, &r))
	must.Eq(t, structs.SecretItems{"password": "top"}, r.Items)
	must.NoError(t, s.RPC("Keyring.List", &structs.KeyringListRootKeyMetaRequest{
		QueryOptions: structs.QueryOptions{Region: "global", AuthToken: root.SecretID}}, &keys))
	must.Len(t, 1, keys.Keys)
	must.Eq(t, activeKeyID, keys.Keys[0].KeyID)
}

// TestNodeSecret_ClientRPCsAllowed checks the RPCs client nodes call with
// their secret ID keep working with ACLs enabled, and that a node is limited
// to its own node where the RPC names one.
func TestNodeSecret_ClientRPCsAllowed(t *testing.T) {
	ci.Parallel(t)

	s, _, cleanup := TestACLServer(t, nil)
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)
	store := s.fsm.State()

	node, other := mock.Node(), mock.Node()
	must.NoError(t, store.UpsertNode(structs.MsgTypeTestSetup, 1000, node))
	must.NoError(t, store.UpsertNode(structs.MsgTypeTestSetup, 1001, other))

	alloc := mock.Alloc()
	alloc.NodeID = node.ID
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, 1002, []*structs.Allocation{alloc}))

	own := &structs.ServiceRegistration{ID: "own", ServiceName: "web", Namespace: structs.DefaultNamespace,
		NodeID: node.ID, Datacenter: "dc1", JobID: alloc.JobID, AllocID: alloc.ID, Address: "10.0.0.1", Port: 80}
	foreign := &structs.ServiceRegistration{ID: "foreign", ServiceName: "web", Namespace: structs.DefaultNamespace,
		NodeID: other.ID, Datacenter: "dc1", JobID: "other", AllocID: uuid.Generate(), Address: "10.0.0.2", Port: 80}
	must.NoError(t, store.UpsertServiceRegistrations(structs.MsgTypeTestSetup, 1003,
		[]*structs.ServiceRegistration{own, foreign}))

	write := structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: node.SecretID}
	query := structs.QueryOptions{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: node.SecretID}

	// Reads clients make.
	var nodeResp structs.SingleNodeResponse
	must.NoError(t, s.RPC("Node.GetNode", &structs.NodeSpecificRequest{NodeID: node.ID, QueryOptions: query}, &nodeResp))
	must.NotNil(t, nodeResp.Node)
	var allocResp structs.SingleAllocResponse
	must.NoError(t, s.RPC("Alloc.GetAlloc", &structs.AllocSpecificRequest{AllocID: alloc.ID, QueryOptions: query}, &allocResp))
	must.NotNil(t, allocResp.Alloc)
	var intentions structs.ServiceIntentionListResponse
	must.NoError(t, s.RPC("ServiceIntention.List", &structs.ServiceIntentionListRequest{QueryOptions: query}, &intentions))
	var services structs.ServiceRegistrationByNameResponse
	must.NoError(t, s.RPC(structs.ServiceRegistrationGetServiceRPCMethod, &structs.ServiceRegistrationByNameRequest{
		ServiceName: "web", QueryOptions: query}, &services))
	must.Len(t, 2, services.Services)

	// A node removes its own registrations, not those of other nodes.
	var delResp structs.ServiceRegistrationDeleteByIDResponse
	must.EqError(t, s.RPC(structs.ServiceRegistrationDeleteByIDRPCMethod,
		&structs.ServiceRegistrationDeleteByIDRequest{ID: foreign.ID, WriteRequest: write}, &delResp),
		structs.ErrPermissionDenied.Error())
	must.NoError(t, s.RPC(structs.ServiceRegistrationDeleteByIDRPCMethod,
		&structs.ServiceRegistrationDeleteByIDRequest{ID: own.ID, WriteRequest: write}, &delResp))
	got, err := store.GetServiceRegistrationByID(nil, structs.DefaultNamespace, own.ID)
	must.NoError(t, err)
	must.Nil(t, got)
	got, err = store.GetServiceRegistrationByID(nil, structs.DefaultNamespace, foreign.ID)
	must.NoError(t, err)
	must.NotNil(t, got)

	// A node drains itself on shutdown (drain_on_shutdown), not other nodes.
	drain := func(nodeID string) error {
		var resp structs.NodeDrainUpdateResponse
		return s.RPC("Node.UpdateDrain", &structs.NodeUpdateDrainRequest{
			NodeID:        nodeID,
			DrainStrategy: &structs.DrainStrategy{DrainSpec: structs.DrainSpec{Deadline: time.Minute}},
			WriteRequest:  write,
		}, &resp)
	}
	must.EqError(t, drain(other.ID), structs.ErrPermissionDenied.Error())
	must.NoError(t, drain(node.ID))
	out, err := store.NodeByID(nil, node.ID)
	must.NoError(t, err)
	must.NotNil(t, out.DrainStrategy)
	out, err = store.NodeByID(nil, other.ID)
	must.NoError(t, err)
	must.Nil(t, out.DrainStrategy)
}

// TestOperator_Snapshot_DeniedStopsAfter403 checks that a denied snapshot
// save or restore returns the 403 and nothing else: no snapshot data after
// it, and no state applied (ST-1). Covers an unknown token, which fails the
// authentication, and a client node's secret ID, which resolves to no ACL.
func TestOperator_Snapshot_DeniedStopsAfter403(t *testing.T) {
	ci.Parallel(t)
	dir := t.TempDir()

	s, _, cleanup := TestACLServer(t, func(c *Config) {
		c.BootstrapExpect = 1
		c.DevMode = false
		c.DataDir = path.Join(dir, "server1")
	})
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)

	node := mock.Node()
	must.NoError(t, s.fsm.State().UpsertNode(structs.MsgTypeTestSetup, 1000, node))

	tokens := map[string]string{
		"invalid token": uuid.Generate(),
		"node secret":   node.SecretID,
	}

	for name, token := range tokens {
		t.Run("save "+name, func(t *testing.T) {
			handler, err := s.StreamingRpcHandler("Operator.SnapshotSave")
			must.NoError(t, err)
			p1, p2 := net.Pipe()
			defer p1.Close()
			defer p2.Close()
			must.NoError(t, p1.SetDeadline(time.Now().Add(30*time.Second)))
			go handler(p2)

			req := structs.SnapshotSaveRequest{}
			req.Region = "global"
			req.AuthToken = token
			must.NoError(t, codec.NewEncoder(p1, structs.MsgpackHandle).Encode(&req))

			var resp structs.SnapshotSaveResponse
			must.NoError(t, codec.NewDecoder(p1, structs.MsgpackHandle).Decode(&resp))
			must.Eq(t, 403, resp.ErrorCode)
			must.Eq(t, structs.ErrPermissionDenied.Error(), resp.ErrorMsg)

			// The handler closes the connection right after the 403.
			rest, err := io.ReadAll(p1)
			must.NoError(t, err)
			must.Len(t, 0, rest, must.Sprintf("%d bytes followed the 403", len(rest)))
		})

		t.Run("restore "+name, func(t *testing.T) {
			snap, job := generateSnapshot(t)

			handler, err := s.StreamingRpcHandler("Operator.SnapshotRestore")
			must.NoError(t, err)
			p1, p2 := net.Pipe()
			defer p1.Close()
			defer p2.Close()
			must.NoError(t, p1.SetDeadline(time.Now().Add(60*time.Second)))
			go handler(p2)

			// Send the request and the snapshot as a client would, without
			// waiting for the response; the writes fail once the server
			// closes the connection.
			go func() {
				encoder := codec.NewEncoder(p1, structs.MsgpackHandle)
				req := structs.SnapshotRestoreRequest{}
				req.Region = "global"
				req.AuthToken = token
				if encoder.Encode(&req) != nil {
					return
				}
				buf := make([]byte, 1024)
				for {
					n, err := snap.Read(buf)
					if n > 0 {
						if encoder.Encode(&cstructs.StreamErrWrapper{Payload: buf[:n]}) != nil {
							return
						}
					}
					if err != nil {
						_ = encoder.Encode(&cstructs.StreamErrWrapper{Error: &cstructs.RpcError{Message: err.Error()}})
						return
					}
				}
			}()

			var resp structs.SnapshotRestoreResponse
			must.NoError(t, codec.NewDecoder(p1, structs.MsgpackHandle).Decode(&resp))
			must.Eq(t, 403, resp.ErrorCode)
			must.Eq(t, structs.ErrPermissionDenied.Error(), resp.ErrorMsg)

			rest, err := io.ReadAll(p1)
			must.NoError(t, err)
			must.Len(t, 0, rest, must.Sprintf("%d bytes followed the 403", len(rest)))

			// The handler has returned: the snapshot was not applied.
			found, err := s.State().JobByID(nil, job.Namespace, job.ID)
			must.NoError(t, err)
			must.Nil(t, found)
		})
	}
}

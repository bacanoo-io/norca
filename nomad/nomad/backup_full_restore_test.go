// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"testing"
	"time"

	"filippo.io/age"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/backup/s3fake"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// fullRestoreTest is a single-server ACL cluster with backups on a fake S3
// store, a second management token and a backup writer.
type fullRestoreTest struct {
	t        *testing.T
	srv      *Server
	root     *structs.ACLToken
	second   *structs.ACLToken
	writer   *structs.ACLToken
	identity *age.X25519Identity

	followers []*Server
}

func newFullRestoreTest(t *testing.T, followers int) (*fullRestoreTest, func()) {
	s3 := s3fake.New()
	identity, err := age.GenerateX25519Identity()
	must.NoError(t, err)

	srv, root, cleanup := TestACLServer(t, func(c *Config) {
		c.DevMode = false
		c.DataDir = t.TempDir()
		c.NumSchedulers = 0
		c.Backup = testBackupConfig(s3, identity, false)
	})
	testutil.WaitForLeader(t, srv.RPC)
	cleanups := []func(){cleanup, s3.Close}

	// Followers join the leader; the bootstrap token was written into the
	// leader's state only, so it stays the leader's.
	ft := &fullRestoreTest{t: t, srv: srv, root: root, identity: identity}
	for range followers {
		follower, cleanup := TestServer(t, func(c *Config) {
			c.DevMode = false
			c.DataDir = t.TempDir()
			c.NumSchedulers = 0
			c.BootstrapExpect = 0
			c.ACLEnabled = true
			c.Backup = testBackupConfig(s3, identity, false)
		})
		cleanups = append(cleanups, cleanup)
		TestJoin(t, srv, follower)
		ft.followers = append(ft.followers, follower)
	}
	for _, f := range ft.followers {
		testutil.WaitForLeader(t, f.RPC)
	}
	must.True(t, srv.IsLeader())
	waitForKeyring(t, srv)

	// Through Raft, so the tokens are in every backup taken after.
	var policyResp structs.GenericResponse
	must.NoError(t, ft.call("ACL.UpsertPolicies", &structs.ACLPolicyUpsertRequest{
		Policies:     []*structs.ACLPolicy{{Name: "backup-write", Rules: `backup { policy = "write" }`}},
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: root.SecretID},
	}, &policyResp))
	var tokenResp structs.ACLTokenUpsertResponse
	must.NoError(t, ft.call(structs.ACLUpsertTokensRPCMethod, &structs.ACLTokenUpsertRequest{
		Tokens: []*structs.ACLToken{
			{Name: "second admin", Type: structs.ACLManagementToken},
			{Name: "backup writer", Type: structs.ACLClientToken, Policies: []string{"backup-write"}},
		},
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: root.SecretID},
	}, &tokenResp))
	ft.second, ft.writer = tokenResp.Tokens[0], tokenResp.Tokens[1]

	return ft, func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
}

// call makes an RPC on a new connection: a full restore replaces the
// server's state and leadership, which a long-lived connection does not need
// to survive.
func (ft *fullRestoreTest) call(method string, args, reply any) error {
	codec := rpcClient(ft.t, ft.srv)
	defer codec.Close()
	return msgpackrpc.CallWithCodec(codec, method, args, reply)
}

func (ft *fullRestoreTest) write(token *structs.ACLToken) structs.WriteRequest {
	return structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: token.SecretID}
}

func (ft *fullRestoreTest) registerJob(job *structs.Job) {
	ft.t.Helper()
	var resp structs.JobRegisterResponse
	must.NoError(ft.t, ft.call("Job.Register", &structs.JobRegisterRequest{Job: job, WriteRequest: ft.write(ft.root)}, &resp))
}

func (ft *fullRestoreTest) setVariable(path, password string) {
	ft.t.Helper()
	var resp structs.VariablesApplyResponse
	must.NoError(ft.t, ft.call(structs.VariablesApplyRPCMethod, &structs.VariablesApplyRequest{
		Op: structs.VarOpSet,
		Var: &structs.VariableDecrypted{
			VariableMetadata: structs.VariableMetadata{Namespace: "default", Path: path},
			Items:            structs.VariableItems{"password": password},
		},
		WriteRequest: ft.write(ft.root),
	}, &resp))
	must.True(ft.t, resp.IsOk())
}

func (ft *fullRestoreTest) readVariable(path string) string {
	ft.t.Helper()
	var resp structs.VariablesReadResponse
	must.NoError(ft.t, ft.call(structs.VariablesReadRPCMethod, &structs.VariablesReadRequest{Path: path,
		QueryOptions: structs.QueryOptions{Region: "global", Namespace: "default", AuthToken: ft.root.SecretID}}, &resp))
	must.NotNil(ft.t, resp.Data)
	return resp.Data.Items["password"]
}

func (ft *fullRestoreTest) save() *structs.BackupRecord {
	ft.t.Helper()
	var resp structs.BackupSaveResponse
	must.NoError(ft.t, ft.call("Backup.Save", &structs.BackupSaveRequest{WriteRequest: ft.write(ft.root)}, &resp))
	must.NotNil(ft.t, resp.Backup)
	return resp.Backup
}

func (ft *fullRestoreTest) request(id string, token *structs.ACLToken, confirm string, dryRun bool) (*structs.BackupRestoreResponse, error) {
	var resp structs.BackupRestoreResponse
	err := ft.call("Backup.Restore", &structs.BackupRestoreRequest{
		ID: id, Mode: structs.BackupRestoreModeFull, Confirm: confirm, DryRun: dryRun,
		AgeIdentity: ft.identity.String(), WriteRequest: ft.write(token),
	}, &resp)
	return &resp, err
}

func (ft *fullRestoreTest) approve(restoreID string, token *structs.ACLToken, identity string) (*structs.BackupRestoreResponse, error) {
	var resp structs.BackupRestoreResponse
	err := ft.call("Backup.ApproveRestore", &structs.BackupRestoreApproveRequest{
		RestoreID: restoreID, AgeIdentity: identity, WriteRequest: ft.write(token),
	}, &resp)
	return &resp, err
}

func (ft *fullRestoreTest) restoreRecord(id string) *structs.BackupRestore {
	ft.t.Helper()
	record, err := ft.srv.fsm.State().BackupRestoreByID(nil, id)
	must.NoError(ft.t, err)
	must.NotNil(ft.t, record)
	return record
}

func TestBackupEndpoint_FullRestore(t *testing.T) {
	ci.Parallel(t)

	ft, cleanup := newFullRestoreTest(t, 0)
	defer cleanup()
	srv := ft.srv

	// The state of the backup: a job and a Variables entry encrypted with
	// the first root key.
	jobA := mock.Job()
	jobA.Meta = map[string]string{"release": "v0"}
	ft.registerJob(jobA.Copy())
	ft.setVariable("app/config", "before")
	oldKey, err := srv.fsm.State().GetActiveRootKeyMeta(nil)
	must.NoError(t, err)
	record := ft.save()
	must.SliceContains(t, record.KeyIDs, oldKey.KeyID)

	// Changes after the backup: a new job, a new version of the first, the
	// Variables entry under a rotated key, and the old key lost from the
	// keyring, so only the keyring of the backup can decrypt the entry.
	jobB := mock.Job()
	ft.registerJob(jobB)
	v1 := jobA.Copy()
	v1.Meta = map[string]string{"release": "v1"}
	ft.registerJob(v1)
	var rotateResp structs.KeyringRotateRootKeyResponse
	must.NoError(t, ft.call("Keyring.Rotate", &structs.KeyringRotateRootKeyRequest{WriteRequest: ft.write(ft.root)}, &rotateResp))
	ft.setVariable("app/config", "after")
	must.NoError(t, srv.encrypter.RemoveKey(oldKey.KeyID))

	// The preview, without touching the cluster.
	preview, err := ft.request(record.ID, ft.root, "", true)
	must.NoError(t, err)
	must.Nil(t, preview.Restore)
	p := preview.Preview
	must.NotNil(t, p)
	must.Eq(t, record.ID, p.BackupID)
	must.Eq(t, 1, p.Counts["jobs/removed"])
	must.Eq(t, 1, p.Counts["jobs/reverted"])
	must.Eq(t, 1, p.Counts["variables/reverted"])
	must.Eq(t, []string{oldKey.KeyID}, p.KeysAdded)
	must.SliceNotEmpty(t, p.Warnings)
	got, err := srv.fsm.State().JobByID(nil, "default", jobB.ID)
	must.NoError(t, err)
	must.NotNil(t, got)

	// The request must be confirmed with the region and needs a management
	// token.
	_, err = ft.request(record.ID, ft.root, "", false)
	must.ErrorContains(t, err, "confirm")
	_, err = ft.request(record.ID, ft.root, "other", false)
	must.ErrorContains(t, err, "confirm")
	_, err = ft.request(record.ID, ft.writer, "global", false)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())

	requested, err := ft.request(record.ID, ft.root, "global", false)
	must.NoError(t, err)
	pending := requested.Restore
	must.NotNil(t, pending)
	must.Eq(t, structs.BackupRestoreStatusPending, pending.Status)
	must.Eq(t, structs.BackupRestoreModeFull, pending.Mode)
	must.Eq(t, "token:"+ft.root.AccessorID, pending.RequestedBy)
	must.True(t, pending.ExpiresAt.After(time.Now()))
	must.NotNil(t, pending.Preview)

	// Only one request waits for approval.
	_, err = ft.request(record.ID, ft.second, "global", false)
	must.ErrorContains(t, err, "waiting for approval")

	// The requester can't approve, nor can a backup writer; a wrong age
	// identity is refused before the approval is recorded.
	_, err = ft.approve(pending.ID, ft.root, ft.identity.String())
	must.ErrorContains(t, err, "requester")
	_, err = ft.approve(pending.ID, ft.writer, ft.identity.String())
	must.EqError(t, err, structs.ErrPermissionDenied.Error())
	other, err := age.GenerateX25519Identity()
	must.NoError(t, err)
	_, err = ft.approve(pending.ID, ft.second, other.String())
	must.ErrorContains(t, err, "does not match")
	must.Eq(t, structs.BackupRestoreStatusPending, ft.restoreRecord(pending.ID).Status)

	// The second administrator approves and the restore runs.
	approved, err := ft.approve(pending.ID, ft.second, ft.identity.String())
	must.NoError(t, err)
	done := approved.Restore
	must.Eq(t, structs.BackupRestoreStatusComplete, done.Status, must.Sprint(done.StatusDescription))
	must.Eq(t, []string{"token:" + ft.second.AccessorID}, done.Approvals)
	must.NotEq(t, "", done.PreRestoreBackupID)

	// The state of the backup is back.
	store := srv.fsm.State()
	got, err = store.JobByID(nil, "default", jobB.ID)
	must.NoError(t, err)
	must.Nil(t, got)
	got, err = store.JobByID(nil, "default", jobA.ID)
	must.NoError(t, err)
	must.NotNil(t, got)
	must.Eq(t, "v0", got.Meta["release"])

	// The keyring of the backup was restored: the entry is readable.
	_, err = srv.encrypter.GetKey(oldKey.KeyID)
	must.NoError(t, err)
	must.Eq(t, "before", ft.readVariable("app/config"))

	// The backup records are those of the store, with the pinned
	// pre-restore backup; the restore record is the audit trail.
	pre, err := store.BackupByID(nil, done.PreRestoreBackupID)
	must.NoError(t, err)
	must.NotNil(t, pre)
	must.Eq(t, structs.BackupTriggerPreRestore, pre.Trigger)
	must.True(t, pre.Pinned)
	must.Eq(t, pending.ID, pre.Labels["restore"])
	original, err := store.BackupByID(nil, record.ID)
	must.NoError(t, err)
	must.NotNil(t, original)
	audit := ft.restoreRecord(pending.ID)
	must.Eq(t, structs.BackupRestoreStatusComplete, audit.Status)
	must.Eq(t, "token:"+ft.root.AccessorID, audit.RequestedBy)
	must.Eq(t, done.Approvals, audit.Approvals)

	// The restore can be undone from the pre-restore backup.
	undo, err := ft.request(pre.ID, ft.second, "global", false)
	must.NoError(t, err)
	must.Eq(t, 1, undo.Preview.Counts["jobs/restored"])
	undone, err := ft.approve(undo.Restore.ID, ft.root, ft.identity.String())
	must.NoError(t, err)
	must.Eq(t, structs.BackupRestoreStatusComplete, undone.Restore.Status, must.Sprint(undone.Restore.StatusDescription))
	got, err = srv.fsm.State().JobByID(nil, "default", jobB.ID)
	must.NoError(t, err)
	must.NotNil(t, got)
	must.Eq(t, "after", ft.readVariable("app/config"))

	// Both restores are in the list.
	var list structs.BackupRestoreListResponse
	must.NoError(t, ft.call("Backup.ListRestores", &structs.BackupRestoreListRequest{
		QueryOptions: structs.QueryOptions{Region: "global", AuthToken: ft.writer.SecretID}}, &list))
	must.Len(t, 2, list.Restores)
	must.Eq(t, undo.Restore.ID, list.Restores[0].ID)
	must.Nil(t, list.Restores[0].Preview.Objects)
}

func TestBackupEndpoint_FullRestoreRejectExpire(t *testing.T) {
	ci.Parallel(t)

	ft, cleanup := newFullRestoreTest(t, 0)
	defer cleanup()
	record := ft.save()

	// The requester withdraws the request.
	requested, err := ft.request(record.ID, ft.root, "global", false)
	must.NoError(t, err)
	var rejectResp structs.BackupRestoreResponse
	must.NoError(t, ft.call("Backup.RejectRestore", &structs.BackupRestoreRejectRequest{
		RestoreID: requested.Restore.ID, Reason: "wrong backup", WriteRequest: ft.write(ft.root),
	}, &rejectResp))
	must.Eq(t, structs.BackupRestoreStatusRejected, rejectResp.Restore.Status)
	rejected := ft.restoreRecord(requested.Restore.ID)
	must.Eq(t, "token:"+ft.root.AccessorID, rejected.RejectedBy)
	must.StrContains(t, rejected.StatusDescription, "wrong backup")
	_, err = ft.approve(requested.Restore.ID, ft.second, ft.identity.String())
	must.ErrorContains(t, err, "rejected")

	// A request that is not approved in time expires.
	requested, err = ft.request(record.ID, ft.root, "global", false)
	must.NoError(t, err)
	stale := ft.restoreRecord(requested.Restore.ID).Copy()
	stale.ExpiresAt = time.Now().Add(-time.Minute)
	index, err := ft.srv.fsm.State().LatestIndex()
	must.NoError(t, err)
	must.NoError(t, ft.srv.fsm.State().UpsertBackupRestores(structs.MsgTypeTestSetup, index+1, []*structs.BackupRestore{stale}))
	_, err = ft.approve(requested.Restore.ID, ft.second, ft.identity.String())
	must.ErrorContains(t, err, "expired")
	must.Eq(t, structs.BackupRestoreStatusExpired, ft.restoreRecord(requested.Restore.ID).Status)

	// An expired request does not block a new one.
	_, err = ft.request(record.ID, ft.second, "global", false)
	must.NoError(t, err)

	// Nothing was restored, so no pre-restore backup was taken.
	records, err := ft.srv.fsm.State().BackupsList(nil)
	must.NoError(t, err)
	must.Len(t, 1, records)
}

func TestBackupEndpoint_FullRestoreNeedsACLs(t *testing.T) {
	ci.Parallel(t)

	s3 := s3fake.New()
	defer s3.Close()
	identity, err := age.GenerateX25519Identity()
	must.NoError(t, err)
	srv, cleanup := TestServer(t, func(c *Config) {
		c.DevMode = false
		c.DataDir = t.TempDir()
		c.NumSchedulers = 0
		c.Backup = testBackupConfig(s3, identity, false)
	})
	defer cleanup()
	codec := rpcClient(t, srv)
	testutil.WaitForLeader(t, srv.RPC)
	waitForKeyring(t, srv)

	var saveResp structs.BackupSaveResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Save", &structs.BackupSaveRequest{
		WriteRequest: structs.WriteRequest{Region: "global"}}, &saveResp))
	err = msgpackrpc.CallWithCodec(codec, "Backup.Restore", &structs.BackupRestoreRequest{
		ID: saveResp.ID, Mode: structs.BackupRestoreModeFull, DryRun: true, AgeIdentity: identity.String(),
		WriteRequest: structs.WriteRequest{Region: "global"},
	}, &structs.BackupRestoreResponse{})
	must.ErrorContains(t, err, "ACLs enabled")
}

func TestKeyringEndpoint_Restore(t *testing.T) {
	ci.Parallel(t)

	s1, cleanup1 := TestServer(t, func(c *Config) { c.NumSchedulers = 0 })
	defer cleanup1()
	s2, cleanup2 := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
		c.BootstrapExpect = 0
	})
	defer cleanup2()
	TestJoin(t, s1, s2)
	testutil.WaitForLeader(t, s1.RPC)
	testutil.WaitForLeader(t, s2.RPC)
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			s1.peerLock.RLock()
			defer s1.peerLock.RUnlock()
			return len(s1.localPeers) == 2
		}),
		wait.Timeout(10*time.Second),
	))

	key, err := structs.NewRootKey(structs.EncryptionAlgorithmAES256GCM)
	must.NoError(t, err)

	// Sent to either server, the key reaches the keystore of both.
	codec := rpcClient(t, s2)
	var resp structs.KeyringRestoreResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Keyring.Restore", &structs.KeyringRestoreRequest{
		Keys: []*structs.RootKey{key}, WriteRequest: structs.WriteRequest{Region: "global"},
	}, &resp))
	must.Eq(t, []string{key.Meta.KeyID}, resp.Added)
	must.Len(t, 2, resp.Servers)
	must.MapEmpty(t, resp.Errors)
	for _, srv := range []*Server{s1, s2} {
		got, err := srv.encrypter.GetKey(key.Meta.KeyID)
		must.NoError(t, err)
		must.Eq(t, key.Key, got)
	}

	// Restoring it again adds nothing; other key material for the same ID
	// is refused.
	resp = structs.KeyringRestoreResponse{}
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Keyring.Restore", &structs.KeyringRestoreRequest{
		Keys: []*structs.RootKey{key}, WriteRequest: structs.WriteRequest{Region: "global"},
	}, &resp))
	must.SliceEmpty(t, resp.Added)
	forged := &structs.RootKey{Meta: key.Meta.Copy(), Key: make([]byte, len(key.Key))}
	err = msgpackrpc.CallWithCodec(codec, "Keyring.Restore", &structs.KeyringRestoreRequest{
		Keys: []*structs.RootKey{forged}, WriteRequest: structs.WriteRequest{Region: "global"},
	}, &structs.KeyringRestoreResponse{})
	must.ErrorContains(t, err, "different key material")
}

func TestKeyringEndpoint_Restore_ACL(t *testing.T) {
	ci.Parallel(t)

	srv, root, cleanup := TestACLServer(t, func(c *Config) { c.NumSchedulers = 0 })
	defer cleanup()
	codec := rpcClient(t, srv)
	testutil.WaitForLeader(t, srv.RPC)

	key, err := structs.NewRootKey(structs.EncryptionAlgorithmAES256GCM)
	must.NoError(t, err)
	writer := mock.CreatePolicyAndToken(t, srv.State(), 1001, "backup-write", `backup { policy = "write" }`)
	err = msgpackrpc.CallWithCodec(codec, "Keyring.Restore", &structs.KeyringRestoreRequest{
		Keys: []*structs.RootKey{key}, WriteRequest: structs.WriteRequest{Region: "global", AuthToken: writer.SecretID},
	}, &structs.KeyringRestoreResponse{})
	must.EqError(t, err, structs.ErrPermissionDenied.Error())

	// LocalOnly is accepted from servers only, but with ACLs it still needs
	// a management token.
	err = msgpackrpc.CallWithCodec(codec, "Keyring.Restore", &structs.KeyringRestoreRequest{
		Keys: []*structs.RootKey{key}, LocalOnly: true,
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: writer.SecretID},
	}, &structs.KeyringRestoreResponse{})
	must.EqError(t, err, structs.ErrPermissionDenied.Error())

	var resp structs.KeyringRestoreResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Keyring.Restore", &structs.KeyringRestoreRequest{
		Keys: []*structs.RootKey{key}, WriteRequest: structs.WriteRequest{Region: "global", AuthToken: root.SecretID},
	}, &resp))
	must.Eq(t, []string{key.Meta.KeyID}, resp.Added)
}

// TestBackupEndpoint_FullRestoreCluster restores a backup into a cluster of
// two servers: the keys of the backup reach the follower with the
// approver's token, and the follower installs the restored state.
func TestBackupEndpoint_FullRestoreCluster(t *testing.T) {
	ci.Parallel(t)

	ft, cleanup := newFullRestoreTest(t, 1)
	defer cleanup()
	leader, follower := ft.srv, ft.followers[0]

	jobA := mock.Job()
	jobA.Meta = map[string]string{"release": "v0"}
	ft.registerJob(jobA.Copy())
	ft.setVariable("app/config", "before")
	oldKey, err := leader.fsm.State().GetActiveRootKeyMeta(nil)
	must.NoError(t, err)
	record := ft.save()

	jobB := mock.Job()
	ft.registerJob(jobB)
	var rotateResp structs.KeyringRotateRootKeyResponse
	must.NoError(t, ft.call("Keyring.Rotate", &structs.KeyringRotateRootKeyRequest{WriteRequest: ft.write(ft.root)}, &rotateResp))
	ft.setVariable("app/config", "after")
	must.Wait(t, wait.InitialSuccess(
		wait.ErrorFunc(func() error { _, err := follower.encrypter.GetKey(rotateResp.Key.KeyID); return err }),
		wait.Timeout(10*time.Second),
	))
	// The old key is lost on both servers.
	must.NoError(t, leader.encrypter.RemoveKey(oldKey.KeyID))
	must.NoError(t, follower.encrypter.RemoveKey(oldKey.KeyID))

	requested, err := ft.request(record.ID, ft.root, "global", false)
	must.NoError(t, err)
	approved, err := ft.approve(requested.Restore.ID, ft.second, ft.identity.String())
	must.NoError(t, err)
	must.Eq(t, structs.BackupRestoreStatusComplete, approved.Restore.Status)
	must.StrNotContains(t, approved.Restore.StatusDescription, "keys not restored")

	// The follower got the keys from the restore and the state from Raft.
	_, err = follower.encrypter.GetKey(oldKey.KeyID)
	must.NoError(t, err)
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			got, err := follower.fsm.State().JobByID(nil, "default", jobB.ID)
			return err == nil && got == nil
		}),
		wait.Timeout(10*time.Second),
	))
	encrypted, err := follower.fsm.State().GetVariable(nil, "default", "app/config")
	must.NoError(t, err)
	must.Eq(t, oldKey.KeyID, encrypted.KeyID)
	items, err := decryptBackupVariable(follower.encrypter, encrypted)
	must.NoError(t, err)
	must.Eq(t, "before", items["password"])
	must.Eq(t, "before", ft.readVariable("app/config"))
}

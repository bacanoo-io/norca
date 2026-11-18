// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"filippo.io/age"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/backup/s3fake"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"
)

// TestBackupEndpoint_RestoreTellsNothingWithoutTheRight checks that a
// selective restore, a dry run included, tells a caller with backup read
// alone nothing about an item it may not write: not whether it exists now
// or in the backup, not its version or modify index, not the number of keys
// of a Variable.
func TestBackupEndpoint_RestoreTellsNothingWithoutTheRight(t *testing.T) {
	ci.Parallel(t)

	s3 := s3fake.New()
	defer s3.Close()
	identity, err := age.GenerateX25519Identity()
	must.NoError(t, err)

	srv, root, cleanup := TestACLServer(t, func(c *Config) {
		c.DevMode = false
		c.DataDir = t.TempDir()
		c.Backup = testBackupConfig(s3, identity, false)
	})
	defer cleanup()
	codec := rpcClient(t, srv)
	testutil.WaitForLeader(t, srv.RPC)
	waitForKeyring(t, srv)

	store := srv.fsm.State()
	reader := mock.CreatePolicyAndToken(t, store, 1001, "backup-read", `backup { policy = "read" }`)
	writer := mock.CreatePolicyAndToken(t, store, 1003, "backup-read-default-write", `
backup { policy = "read" }
namespace "default" {
  capabilities = ["submit-job", "read-job"]
  variables {
    path "secret/*" { capabilities = ["write", "read"] }
  }
}`)

	var varResp structs.VariablesApplyResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.VariablesApplyRPCMethod, &structs.VariablesApplyRequest{
		Op: structs.VarOpSet,
		Var: &structs.VariableDecrypted{
			VariableMetadata: structs.VariableMetadata{Namespace: "default", Path: "secret/app"},
			Items:            map[string]string{"aws_secret_key": "x", "aws_access_key": "y"},
		},
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: root.SecretID},
	}, &varResp))
	job := mock.Job()
	var regResp structs.JobRegisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", &structs.JobRegisterRequest{Job: job,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace, AuthToken: root.SecretID}}, &regResp))

	var saveResp structs.BackupSaveResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Save", &structs.BackupSaveRequest{
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: root.SecretID}}, &saveResp))

	items := func() []*structs.BackupRestoreItem {
		return []*structs.BackupRestoreItem{
			{Type: structs.BackupRestoreItemJob, ID: job.ID},
			{Type: structs.BackupRestoreItemJob, ID: "not-in-the-backup"},
			{Type: structs.BackupRestoreItemVariable, ID: "secret/app"},
			{Type: structs.BackupRestoreItemVariable, ID: "secret/not-in-the-backup"},
			{Type: structs.BackupRestoreItemNamespace, ID: "default"},
			{Type: structs.BackupRestoreItemACLPolicy, ID: "backup-read"},
			{Type: structs.BackupRestoreItemACLRole, ID: "no-such-role"},
			{Type: structs.BackupRestoreItemQuota, ID: "no-such-quota"},
			{Type: structs.BackupRestoreItemAdmissionPolicy, ID: "no-such-policy"},
			{Type: structs.BackupRestoreItemNetworkPolicy, ID: "no-such-policy"},
			{Type: structs.BackupRestoreItemAdminNetworkPolicy, ID: "no-such-policy"},
			{Type: structs.BackupRestoreItemServiceIntention, ID: "default/web -> db"},
			{Type: structs.BackupRestoreItemExternalService, ID: "no-such-service"},
			{Type: structs.BackupRestoreItemCustomObject, ID: "db.example.com/PostgresCluster/billing"},
			{Type: structs.BackupRestoreItemConsulKV, ID: "app/config"},
			{Type: structs.BackupRestoreItemSecret, ID: "app/db"},
		}
	}
	restore := func(token string, dryRun bool) []*structs.BackupRestoreItemResult {
		var resp structs.BackupRestoreResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Restore", &structs.BackupRestoreRequest{
			ID: saveResp.ID, AgeIdentity: identity.String(), DryRun: dryRun, Items: items(),
			WriteRequest: structs.WriteRequest{Region: "global", AuthToken: token}}, &resp))
		must.Len(t, len(items()), resp.Results)
		return resp.Results
	}

	// Backup read alone: every item is refused the same way, in a dry run
	// and in a real one.
	for _, dryRun := range []bool{true, false} {
		for _, res := range restore(reader.SecretID, dryRun) {
			what := must.Sprintf("dry run %v, %s %q: action %q, current index %d, description %q, error %q",
				dryRun, res.Item.Type, res.Item.ID, res.Action, res.CurrentIndex, res.Description, res.Error)
			must.Eq(t, structs.ErrPermissionDenied.Error(), res.Error, what)
			must.Eq(t, "", res.Action, what)
			must.Eq(t, uint64(0), res.CurrentIndex, what)
			must.Eq(t, "", res.Description, what)
		}
	}

	// With the right to write the job and the Variable their preview is
	// given; the rest stays refused.
	// (The results are in the order of the restore, not of the request.)
	find := func(results []*structs.BackupRestoreItemResult, typ, id string) *structs.BackupRestoreItemResult {
		for _, res := range results {
			if res.Item.Type == typ && res.Item.ID == id {
				return res
			}
		}
		t.Fatalf("no result of %s %q", typ, id)
		return nil
	}
	results := restore(writer.SecretID, true)
	allowed := 0
	for _, res := range results {
		switch {
		case res.Item.Type == structs.BackupRestoreItemJob && res.Item.ID == job.ID:
			must.Eq(t, "", res.Error)
			must.Eq(t, "update", res.Action)
			must.Positive(t, res.CurrentIndex)
		case res.Item.Type == structs.BackupRestoreItemVariable && res.Item.ID == "secret/app":
			must.Eq(t, "", res.Error)
			must.Eq(t, "update", res.Action)
			must.Positive(t, res.CurrentIndex)
			must.StrContains(t, res.Description, "(2 keys)")
		case res.Item.Type == structs.BackupRestoreItemJob, res.Item.Type == structs.BackupRestoreItemVariable:
			must.StrContains(t, res.Error, "not found in the backup")
		default:
			must.Eq(t, structs.ErrPermissionDenied.Error(), res.Error, must.Sprintf("%s %q", res.Item.Type, res.Item.ID))
			must.Eq(t, "", res.Action)
			continue
		}
		allowed++
	}
	must.Eq(t, 4, allowed)

	// A management token gets the preview of the operator level items.
	results = restore(root.SecretID, true)
	res := find(results, structs.BackupRestoreItemNamespace, "default")
	must.Eq(t, "update", res.Action, must.Sprint(res.Error))
	res = find(results, structs.BackupRestoreItemACLPolicy, "backup-read")
	must.Eq(t, "update", res.Action, must.Sprint(res.Error))
	res = find(results, structs.BackupRestoreItemACLRole, "no-such-role")
	must.StrContains(t, res.Error, "not found in the backup")
}

// The record of a selective restore lists its items and their results:
// GetRestore and ListRestores give a caller without a management token only
// those it may read, by the rules of Backup.Contents (review finding SRV-9).
func TestBackupEndpoint_RestoreRecordItemsFiltered(t *testing.T) {
	ci.Parallel(t)

	s3 := s3fake.New()
	defer s3.Close()
	identity, err := age.GenerateX25519Identity()
	must.NoError(t, err)

	srv, root, cleanup := TestACLServer(t, func(c *Config) {
		c.DevMode = false
		c.DataDir = t.TempDir()
		c.Backup = testBackupConfig(s3, identity, false)
	})
	defer cleanup()
	codec := rpcClient(t, srv)
	testutil.WaitForLeader(t, srv.RPC)
	waitForKeyring(t, srv)

	store := srv.fsm.State()
	reader := mock.CreatePolicyAndToken(t, store, 1001, "backup-read-default", `
backup { policy = "read" }
namespace "default" {
  capabilities = ["read-job"]
  variables {
    path "secret/*" { capabilities = ["list"] }
  }
}`)

	job := mock.Job()
	var regResp structs.JobRegisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", &structs.JobRegisterRequest{Job: job,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace, AuthToken: root.SecretID}}, &regResp))
	var saveResp structs.BackupSaveResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Save", &structs.BackupSaveRequest{
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: root.SecretID}}, &saveResp))

	items := []*structs.BackupRestoreItem{
		{Type: structs.BackupRestoreItemJob, ID: job.ID},
		{Type: structs.BackupRestoreItemVariable, ID: "secret/app"},
		{Type: structs.BackupRestoreItemJob, Namespace: "platform", ID: "payments"},
		{Type: structs.BackupRestoreItemVariable, Namespace: "platform", ID: "secret/db-admin"},
		{Type: structs.BackupRestoreItemVariable, ID: "internal/keys"},
		{Type: structs.BackupRestoreItemACLPolicy, ID: "backup-read-default"},
	}
	var restoreResp structs.BackupRestoreResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Restore", &structs.BackupRestoreRequest{
		ID: saveResp.ID, AgeIdentity: identity.String(), Items: items,
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: root.SecretID}}, &restoreResp))
	must.NotNil(t, restoreResp.Restore)
	id := restoreResp.Restore.ID

	get := func(token string) *structs.BackupRestore {
		var resp structs.SingleBackupRestoreResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.GetRestore", &structs.BackupRestoreSpecificRequest{
			RestoreID: id, QueryOptions: structs.QueryOptions{Region: "global", AuthToken: token},
		}, &resp))
		must.NotNil(t, resp.Restore)
		return resp.Restore
	}
	list := func(token string) *structs.BackupRestore {
		var resp structs.BackupRestoreListResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.ListRestores", &structs.BackupRestoreListRequest{
			QueryOptions: structs.QueryOptions{Region: "global", AuthToken: token},
		}, &resp))
		for _, r := range resp.Restores {
			if r.ID == id {
				return r
			}
		}
		t.Fatalf("restore %s is not listed", id)
		return nil
	}
	seen := func(r *structs.BackupRestore) (fromItems, fromResults []string) {
		for _, item := range r.Items {
			fromItems = append(fromItems, item.String())
		}
		for _, res := range r.Results {
			fromResults = append(fromResults, res.Item.String())
		}
		return fromItems, fromResults
	}
	readable := []string{"job default/" + job.ID, "variable default/secret/app"}

	for name, read := range map[string]func(string) *structs.BackupRestore{"get": get, "list": list} {
		fromItems, fromResults := seen(read(reader.SecretID))
		must.Eq(t, readable, fromItems, must.Sprintf("%s: items", name))
		must.SliceContainsAll(t, readable, fromResults, must.Sprintf("%s: results", name))
		// The status counts what the caller sees (review finding SEC-30):
		// the Variable is not in the backup, the job is restored.
		must.Eq(t, "1 of 2 items failed", read(reader.SecretID).StatusDescription,
			must.Sprintf("%s: status description", name))

		fromItems, fromResults = seen(read(root.SecretID))
		must.Len(t, len(items), fromItems, must.Sprintf("%s: items of the management token", name))
		must.Len(t, len(items), fromResults, must.Sprintf("%s: results of the management token", name))
		must.Eq(t, "4 of 6 items failed", read(root.SecretID).StatusDescription,
			must.Sprintf("%s: status description of the management token", name))
	}

	// The stored record keeps everything.
	stored, err := store.BackupRestoreByID(nil, id)
	must.NoError(t, err)
	must.Len(t, len(items), stored.Items)
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/backup"
	"github.com/hashicorp/nomad/nomad/backup/s3fake"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// waitForKeyring waits until the leader has its active root key.
func waitForKeyring(t *testing.T, srv *Server) {
	t.Helper()
	must.Wait(t, wait.InitialSuccess(
		wait.ErrorFunc(func() error { _, err := srv.backupKeyring(); return err }),
		wait.Timeout(10*time.Second),
		wait.Gap(50*time.Millisecond),
	))
}

// waitForBackup waits until the backup_status table has n records and
// returns the newest.
func waitForBackup(t *testing.T, srv *Server, n int) *structs.BackupRecord {
	t.Helper()
	var record *structs.BackupRecord
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			records, err := srv.State().BackupsList(nil)
			if err != nil || len(records) < n {
				return false
			}
			record = records[0]
			return true
		}),
		wait.Timeout(30*time.Second),
		wait.Gap(100*time.Millisecond),
	))
	must.Eq(t, structs.BackupStatusComplete, record.Status, must.Sprint(record.StatusDescription))
	return record
}

func TestBackupEndpoint_BrowseRestore(t *testing.T) {
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

	// A job with two versions.
	job := mock.Job()
	job.Meta = map[string]string{"release": "v0"}
	register := func(job *structs.Job) {
		req := &structs.JobRegisterRequest{Job: job,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace}}
		var resp structs.JobRegisterResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", req, &resp))
	}
	register(job.Copy())
	v1 := job.Copy()
	v1.Meta = map[string]string{"release": "v1"}
	register(v1)

	// Deployment and allocation history of the job.
	store := srv.fsm.State()
	stored, err := store.JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	deployment := mock.Deployment()
	deployment.JobID = job.ID
	deployment.JobVersion = 1
	alloc := mock.Alloc()
	alloc.JobID = job.ID
	alloc.Job = stored
	alloc.DeploymentID = deployment.ID
	index, _ := store.LatestIndex()
	must.NoError(t, store.UpsertDeployment(index+1, deployment))
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, index+2, []*structs.Allocation{alloc}))

	// A Variables entry.
	setVar := &structs.VariablesApplyRequest{
		Op: structs.VarOpSet,
		Var: &structs.VariableDecrypted{
			VariableMetadata: structs.VariableMetadata{Namespace: "default", Path: "nomad/jobs/" + job.ID},
			Items:            structs.VariableItems{"password": "s3cr3t", "user": "app"},
		},
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default"},
	}
	var setResp structs.VariablesApplyResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.VariablesApplyRPCMethod, setVar, &setResp))
	must.True(t, setResp.IsOk())

	// Enable the schedule: the first scheduled backup is due immediately.
	cfg := testBackupConfig(s3, identity, true)
	must.NoError(t, srv.backup.SetConfig(cfg))
	record := waitForBackup(t, srv, 1)
	must.Eq(t, structs.BackupTriggerSchedule, record.Trigger)
	must.Eq(t, 1, record.Summary["jobs"])
	must.Eq(t, 1, record.Summary["variables"])

	// List shows it with the schedule.
	var list structs.BackupListResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.List",
		&structs.BackupListRequest{QueryOptions: structs.QueryOptions{Region: "global"}}, &list))
	must.Len(t, 1, list.Backups)
	must.True(t, list.Schedule.Enabled)
	must.Eq(t, record.ID, list.Schedule.LastSuccessID)

	// Purge the job and delete the variable.
	dereg := &structs.JobDeregisterRequest{JobID: job.ID, Purge: true,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace}}
	var deregResp structs.JobDeregisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Deregister", dereg, &deregResp))
	delVar := &structs.VariablesApplyRequest{
		Op:           structs.VarOpDelete,
		Var:          &structs.VariableDecrypted{VariableMetadata: structs.VariableMetadata{Namespace: "default", Path: "nomad/jobs/" + job.ID}},
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default"},
	}
	var delResp structs.VariablesApplyResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.VariablesApplyRPCMethod, delVar, &delResp))

	contents := func(contentType, jobID string) *structs.BackupContents {
		t.Helper()
		req := &structs.BackupContentsRequest{ID: record.ID, Type: contentType, JobID: jobID,
			AgeIdentity:  identity.String(),
			QueryOptions: structs.QueryOptions{Region: "global", Namespace: "*"}}
		var resp structs.BackupContentsResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Contents", req, &resp))
		return resp.Contents
	}

	// Browse the backup: jobs with their versions, deployments, allocations,
	// and Variables without values.
	jobs := contents(structs.BackupContentJobs, "")
	must.Len(t, 1, jobs.Jobs)
	must.Eq(t, job.ID, jobs.Jobs[0].ID)
	must.Eq(t, []uint64{1, 0}, jobs.Jobs[0].Versions)

	versions := contents(structs.BackupContentJobVersions, job.ID)
	must.Len(t, 2, versions.JobVersions)
	must.Eq(t, "v1", versions.JobVersions[0].Meta["release"])

	deployments := contents(structs.BackupContentDeployments, job.ID)
	must.Len(t, 1, deployments.Deployments)
	must.Eq(t, deployment.ID, deployments.Deployments[0].ID)

	allocs := contents(structs.BackupContentAllocations, "")
	must.Len(t, 1, allocs.Allocations)
	must.Eq(t, alloc.ID, allocs.Allocations[0].ID)

	vars := contents(structs.BackupContentVariables, "")
	must.Len(t, 1, vars.Variables)
	must.Eq(t, []string{"password", "user"}, vars.Variables[0].Keys)
	buf, err := json.Marshal(vars)
	must.NoError(t, err)
	must.StrNotContains(t, string(buf), "s3cr3t")

	namespaces := contents(structs.BackupContentNamespaces, "")
	must.SliceNotEmpty(t, namespaces.Namespaces)

	// The diff against the current state shows the deleted objects.
	var diffResp structs.BackupDiffResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Diff", &structs.BackupDiffRequest{
		ID: record.ID, AgeIdentity: identity.String(),
		QueryOptions: structs.QueryOptions{Region: "global", Namespace: "*"},
	}, &diffResp))
	changes := map[string]string{}
	for _, d := range diffResp.Diff.Objects {
		changes[d.Type+"/"+d.ID] = d.Change
	}
	must.Eq(t, structs.BackupDiffDeleted, changes["jobs/"+job.ID])
	must.Eq(t, structs.BackupDiffDeleted, changes["variables/nomad/jobs/"+job.ID])
	must.Eq(t, structs.BackupDiffDeleted, changes["job-versions/"+job.ID+"@0"])
	must.Eq(t, "current", diffResp.Diff.To)

	// A wrong identity can't read the backup.
	other, err := age.GenerateX25519Identity()
	must.NoError(t, err)
	err = msgpackrpc.CallWithCodec(codec, "Backup.Contents", &structs.BackupContentsRequest{
		ID: record.ID, AgeIdentity: other.String(),
		QueryOptions: structs.QueryOptions{Region: "global"},
	}, &structs.BackupContentsResponse{})
	must.ErrorContains(t, err, "does not match")
	err = msgpackrpc.CallWithCodec(codec, "Backup.Contents", &structs.BackupContentsRequest{
		ID: record.ID, QueryOptions: structs.QueryOptions{Region: "global"},
	}, &structs.BackupContentsResponse{})
	must.ErrorContains(t, err, "age identity is required")

	// Selective restore: preview first.
	items := []*structs.BackupRestoreItem{
		{Type: structs.BackupRestoreItemJob, Namespace: "default", ID: job.ID, Version: pointer.Of(uint64(0))},
		{Type: structs.BackupRestoreItemVariable, Namespace: "default", ID: "nomad/jobs/" + job.ID},
	}
	restoreReq := &structs.BackupRestoreRequest{ID: record.ID, Items: items, DryRun: true,
		AgeIdentity: identity.String(), WriteRequest: structs.WriteRequest{Region: "global"}}
	var restoreResp structs.BackupRestoreResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Restore", restoreReq, &restoreResp))
	must.Len(t, 2, restoreResp.Results)
	must.Eq(t, "create", restoreResp.Results[0].Action)
	must.Eq(t, "create", restoreResp.Results[1].Action)
	must.Nil(t, restoreResp.Restore)
	got, err := store.JobByID(nil, "default", job.ID)
	must.NoError(t, err)
	must.Nil(t, got)

	// Then restore.
	restoreReq.DryRun = false
	restoreResp = structs.BackupRestoreResponse{}
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Restore", restoreReq, &restoreResp))
	for _, r := range restoreResp.Results {
		must.Eq(t, "", r.Error)
		must.Positive(t, r.Index)
	}
	must.Eq(t, structs.BackupRestoreStatusComplete, restoreResp.Restore.Status)

	got, err = store.JobByID(nil, "default", job.ID)
	must.NoError(t, err)
	must.NotNil(t, got)
	must.Eq(t, "v0", got.Meta["release"])

	readReq := &structs.VariablesReadRequest{Path: "nomad/jobs/" + job.ID,
		QueryOptions: structs.QueryOptions{Region: "global", Namespace: "default"}}
	var readResp structs.VariablesReadResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, structs.VariablesReadRPCMethod, readReq, &readResp))
	must.NotNil(t, readResp.Data)
	must.Eq(t, "s3cr3t", readResp.Data.Items["password"])

	restoreRecord, err := store.BackupRestoreByID(nil, restoreResp.Restore.ID)
	must.NoError(t, err)
	must.Eq(t, structs.BackupRestoreStatusComplete, restoreRecord.Status)

	// Restoring the variable again without a check index is refused, with
	// the current index to use.
	restoreReq.Items = items[1:]
	restoreResp = structs.BackupRestoreResponse{}
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Restore", restoreReq, &restoreResp))
	must.StrContains(t, restoreResp.Results[0].Error, "check index")
	must.Eq(t, structs.BackupRestoreStatusFailed, restoreResp.Restore.Status)
	restoreReq.Items[0].CheckIndex = pointer.Of(restoreResp.Results[0].CurrentIndex)
	restoreResp = structs.BackupRestoreResponse{}
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Restore", restoreReq, &restoreResp))
	must.Eq(t, "", restoreResp.Results[0].Error)

	// A full restore needs ACLs for its two-person approval.
	err = msgpackrpc.CallWithCodec(codec, "Backup.Restore", &structs.BackupRestoreRequest{
		ID: record.ID, Mode: structs.BackupRestoreModeFull, AgeIdentity: identity.String(),
		WriteRequest: structs.WriteRequest{Region: "global"},
	}, &structs.BackupRestoreResponse{})
	must.ErrorContains(t, err, "ACLs enabled")
}

func TestBackupEndpoint_SaveUpdateDelete(t *testing.T) {
	ci.Parallel(t)

	s3 := s3fake.New()
	defer s3.Close()
	identity, err := age.GenerateX25519Identity()
	must.NoError(t, err)

	srv, cleanup := TestServer(t, func(c *Config) {
		c.DevMode = false
		c.DataDir = t.TempDir()
		c.Backup = testBackupConfig(s3, identity, false)
		// Only the newest backup and pinned ones are kept.
		c.Backup.Retention = &backup.RetentionConfig{MinKeep: 1}
	})
	defer cleanup()
	codec := rpcClient(t, srv)
	testutil.WaitForLeader(t, srv.RPC)
	waitForKeyring(t, srv)

	save := func(detach bool) *structs.BackupSaveResponse {
		var resp structs.BackupSaveResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Save", &structs.BackupSaveRequest{
			Labels: map[string]string{"note": "manual"}, Detach: detach, Pin: !detach,
			WriteRequest: structs.WriteRequest{Region: "global"},
		}, &resp))
		return &resp
	}
	first := save(false)
	must.NotNil(t, first.Backup)
	must.Eq(t, structs.BackupTriggerManual, first.Backup.Trigger)
	must.Eq(t, "manual", first.Backup.Labels["note"])
	must.True(t, first.Backup.Pinned)

	detached := save(true)
	must.NotEq(t, "", detached.ID)
	must.Nil(t, detached.Backup)
	waitForBackup(t, srv, 2)

	// Keep the first backup pinned and drop its label.
	var upd structs.BackupUpdateResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Update", &structs.BackupUpdateRequest{
		ID: first.ID, Pinned: pointer.Of(true), RemoveLabels: []string{"note"},
		WriteRequest: structs.WriteRequest{Region: "global"},
	}, &upd))
	must.True(t, upd.Backup.Pinned)
	must.MapEmpty(t, upd.Backup.Labels)
	manifest := s3.Object("backups", "norca/global/"+upd.Backup.Path+"/"+backup.ManifestKey)
	must.True(t, strings.Contains(string(manifest), `"pinned": true`))

	var get structs.SingleBackupResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Get", &structs.BackupSpecificRequest{
		ID: first.ID, QueryOptions: structs.QueryOptions{Region: "global"},
	}, &get))
	must.True(t, get.Backup.Pinned)

	// A pinned backup and the min_keep newest can't be deleted.
	del := func(id string) error {
		return msgpackrpc.CallWithCodec(codec, "Backup.Delete", &structs.BackupDeleteRequest{
			IDs: []string{id}, WriteRequest: structs.WriteRequest{Region: "global"},
		}, &structs.GenericResponse{})
	}
	must.ErrorContains(t, del(first.ID), "pinned")
	must.ErrorContains(t, del(detached.ID), "min_keep")
	must.ErrorContains(t, del("20200101T000000Z-1"), "not found")

	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Update", &structs.BackupUpdateRequest{
		ID: first.ID, Pinned: pointer.Of(false), WriteRequest: structs.WriteRequest{Region: "global"},
	}, &upd))
	must.NoError(t, del(first.ID))
	for _, k := range s3.Keys("backups") {
		must.StrNotContains(t, k, first.ID)
	}

	// A download URL of the state object.
	var url structs.BackupDownloadURLResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.DownloadURL", &structs.BackupDownloadURLRequest{
		ID: detached.ID, QueryOptions: structs.QueryOptions{Region: "global"},
	}, &url))
	must.StrContains(t, url.URL, backup.StateKey)
}

func TestBackupEndpoint_ACL(t *testing.T) {
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
	nobody := mock.CreatePolicyAndToken(t, store, 1002, "nothing", `node { policy = "read" }`)

	// A job, backed up by the management token.
	job := mock.Job()
	var regResp structs.JobRegisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", &structs.JobRegisterRequest{Job: job,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace, AuthToken: root.SecretID}}, &regResp))

	var saveResp structs.BackupSaveResponse
	err = msgpackrpc.CallWithCodec(codec, "Backup.Save", &structs.BackupSaveRequest{
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: reader.SecretID}}, &saveResp)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Save", &structs.BackupSaveRequest{
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: root.SecretID}}, &saveResp))

	// Listing needs backup read.
	err = msgpackrpc.CallWithCodec(codec, "Backup.List", &structs.BackupListRequest{
		QueryOptions: structs.QueryOptions{Region: "global", AuthToken: nobody.SecretID}}, &structs.BackupListResponse{})
	must.EqError(t, err, structs.ErrPermissionDenied.Error())
	var list structs.BackupListResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.List", &structs.BackupListRequest{
		QueryOptions: structs.QueryOptions{Region: "global", AuthToken: reader.SecretID}}, &list))
	must.Len(t, 1, list.Backups)

	// Browsing hides the jobs of namespaces the token can't read.
	var contents structs.BackupContentsResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Contents", &structs.BackupContentsRequest{
		ID: saveResp.ID, AgeIdentity: identity.String(),
		QueryOptions: structs.QueryOptions{Region: "global", Namespace: "*", AuthToken: reader.SecretID}}, &contents))
	must.Len(t, 0, contents.Contents.Jobs)

	// Restoring needs the namespace permission too: the inner Job.Register
	// runs with the caller's token.
	var restoreResp structs.BackupRestoreResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Restore", &structs.BackupRestoreRequest{
		ID: saveResp.ID, AgeIdentity: identity.String(),
		Items:        []*structs.BackupRestoreItem{{Type: structs.BackupRestoreItemJob, ID: job.ID}},
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: reader.SecretID}}, &restoreResp))
	must.StrContains(t, restoreResp.Results[0].Error, structs.ErrPermissionDenied.Error())
}

// TestBackupEndpoint_Directory backs up into a directory destination,
// browses and restores from it, and checks the missing download URL and the
// error after a leader change to a server that does not share the directory.
func TestBackupEndpoint_Directory(t *testing.T) {
	ci.Parallel(t)

	identity, err := age.GenerateX25519Identity()
	must.NoError(t, err)
	backupDir := t.TempDir()

	srv, cleanup := TestServer(t, func(c *Config) {
		c.DevMode = false
		c.DataDir = t.TempDir()
		c.NumSchedulers = 0
		c.Backup = backup.DefaultConfig()
		c.Backup.Encryption.Recipients = []string{identity.Recipient().String()}
		c.Backup.BrowseCacheTTL = 0
		c.Backup.Destinations = []*backup.DestinationConfig{{
			Name: "local", Type: backup.DestinationTypeDirectory, Path: backupDir, Prune: true,
		}}
	})
	defer cleanup()
	codec := rpcClient(t, srv)
	testutil.WaitForLeader(t, srv.RPC)
	waitForKeyring(t, srv)

	job := mock.Job()
	var regResp structs.JobRegisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", &structs.JobRegisterRequest{Job: job,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace}}, &regResp))

	var saveResp structs.BackupSaveResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Save", &structs.BackupSaveRequest{
		WriteRequest: structs.WriteRequest{Region: "global"}}, &saveResp))
	must.Eq(t, structs.BackupStatusComplete, saveResp.Backup.Status)
	must.StrHasPrefix(t, "file://", saveResp.Backup.Destinations[0].Location)
	folder := filepath.Join(backupDir, "norca", "global", filepath.FromSlash(saveResp.Backup.Path))
	_, err = os.Stat(filepath.Join(folder, backup.ManifestKey))
	must.NoError(t, err)

	// Browse it.
	contentsReq := &structs.BackupContentsRequest{ID: saveResp.ID, AgeIdentity: identity.String(),
		QueryOptions: structs.QueryOptions{Region: "global", Namespace: "*"}}
	var contents structs.BackupContentsResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Contents", contentsReq, &contents))
	must.Len(t, 1, contents.Contents.Jobs)

	// Restore the purged job from it.
	var deregResp structs.JobDeregisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Deregister", &structs.JobDeregisterRequest{JobID: job.ID, Purge: true,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace}}, &deregResp))
	var restoreResp structs.BackupRestoreResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Backup.Restore", &structs.BackupRestoreRequest{
		ID: saveResp.ID, AgeIdentity: identity.String(),
		Items:        []*structs.BackupRestoreItem{{Type: structs.BackupRestoreItemJob, Namespace: job.Namespace, ID: job.ID}},
		WriteRequest: structs.WriteRequest{Region: "global"}}, &restoreResp))
	must.Eq(t, structs.BackupRestoreStatusComplete, restoreResp.Restore.Status)
	got, err := srv.fsm.State().JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.NotNil(t, got)

	// No pre-signed URL: a bad request naming the file.
	err = msgpackrpc.CallWithCodec(codec, "Backup.DownloadURL", &structs.BackupDownloadURLRequest{
		ID: saveResp.ID, QueryOptions: structs.QueryOptions{Region: "global"},
	}, &structs.BackupDownloadURLResponse{})
	must.ErrorContains(t, err, "not available for directory destinations")
	must.ErrorContains(t, err, backup.StateKey)

	// After a leader change to a server with its own directory the backup is
	// missing; the error names the shared storage requirement.
	must.NoError(t, os.RemoveAll(filepath.Join(backupDir, "norca")))
	err = msgpackrpc.CallWithCodec(codec, "Backup.Contents", contentsReq, &structs.BackupContentsResponse{})
	must.ErrorContains(t, err, "shared storage")
}

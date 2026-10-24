// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/backup"
	"github.com/hashicorp/nomad/nomad/backup/s3fake"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// testBackupConfig returns a backup configuration with one destination on
// the fake S3 server, encrypted for the identity.
func testBackupConfig(s3 *s3fake.Server, identity *age.X25519Identity, enabled bool) *backup.Config {
	cfg := backup.DefaultConfig()
	cfg.Enabled = enabled
	cfg.VerifyChecksum = true
	cfg.Encryption.Recipients = []string{identity.Recipient().String()}
	cfg.Destinations = []*backup.DestinationConfig{{
		Name:           "primary",
		Type:           backup.DestinationTypeS3,
		Endpoint:       s3.Endpoint(),
		Bucket:         "backups",
		Prefix:         "norca/${region}",
		Region:         "us-east-1",
		ForcePathStyle: true,
		AccessKey:      "test",
		SecretKey:      "testsecret",
		Prune:          true,
	}}
	return cfg
}

func TestBackup_Scheduled(t *testing.T) {
	ci.Parallel(t)

	s3 := s3fake.New()
	defer s3.Close()
	identity, err := age.GenerateX25519Identity()
	must.NoError(t, err)

	srv, cleanup := TestServer(t, func(c *Config) {
		// The in-memory snapshot store of dev mode can't be opened.
		c.DevMode = false
		c.DataDir = t.TempDir()
		c.Backup = testBackupConfig(s3, identity, true)
	})
	defer cleanup()
	testutil.WaitForLeader(t, srv.RPC)

	var record *structs.BackupRecord
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			records, err := srv.State().BackupsList(nil)
			if err != nil || len(records) == 0 {
				return false
			}
			record = records[0]
			return true
		}),
		wait.Timeout(30*time.Second),
		wait.Gap(100*time.Millisecond),
	))
	must.Eq(t, structs.BackupStatusComplete, record.Status, must.Sprint(record.StatusDescription))
	must.Eq(t, structs.BackupTriggerSchedule, record.Trigger)
	must.Eq(t, srv.Region(), record.Region)
	must.Positive(t, record.RaftIndex)
	must.Positive(t, record.RaftTerm)
	must.Len(t, 1, record.KeyIDs)

	prefix := "norca/" + srv.Region() + "/" + record.Path + "/"
	keys := s3.Keys("backups")
	must.SliceContains(t, keys, prefix+backup.ManifestKey)
	must.SliceContains(t, keys, prefix+backup.StateKey)
	must.SliceContains(t, keys, prefix+backup.KeyringKey)
	must.SliceContains(t, keys, "norca/"+srv.Region()+"/"+backup.LatestKey)
	must.True(t, strings.Contains(string(s3.Object("backups", "norca/"+srv.Region()+"/"+backup.LatestKey)), record.ID))
}

func TestFSM_SnapshotRestore_Backups(t *testing.T) {
	ci.Parallel(t)
	fsm := testFSM(t)

	record := &structs.BackupRecord{
		ID:      backup.NewID(time.Now(), 10),
		Status:  structs.BackupStatusComplete,
		Summary: map[string]int{"jobs": 3},
		Destinations: []*structs.BackupDestinationStatus{{
			Name: "primary", Status: structs.BackupDestinationStatusVerified,
			Objects: map[string]*structs.BackupObject{"state": {Key: backup.StateKey, Size: 10}},
		}},
	}
	must.NoError(t, fsm.State().UpsertBackups(structs.MsgTypeTestSetup, 1000, []*structs.BackupRecord{record}))
	restore := &structs.BackupRestore{ID: "r1", BackupID: record.ID, Mode: structs.BackupRestoreModeSelective,
		Status: structs.BackupRestoreStatusComplete}
	must.NoError(t, fsm.State().UpsertBackupRestores(structs.MsgTypeTestSetup, 1001, []*structs.BackupRestore{restore}))

	fsm2 := testSnapshotRestore(t, fsm)
	got, err := fsm2.State().BackupByID(nil, record.ID)
	must.NoError(t, err)
	must.Eq(t, record, got)
	gotRestore, err := fsm2.State().BackupRestoreByID(nil, "r1")
	must.NoError(t, err)
	must.Eq(t, restore, gotRestore)
}

func TestFSM_ApplyBackups(t *testing.T) {
	ci.Parallel(t)
	fsm := testFSM(t)

	record := &structs.BackupRecord{ID: backup.NewID(time.Now(), 10), Status: structs.BackupStatusComplete}
	buf, err := structs.Encode(structs.BackupUpsertRequestType|structs.IgnoreUnknownTypeFlag,
		&structs.BackupUpsertRequest{Backups: []*structs.BackupRecord{record}})
	must.NoError(t, err)
	must.Nil(t, fsm.Apply(makeLog(buf)))

	got, err := fsm.State().BackupByID(nil, record.ID)
	must.NoError(t, err)
	must.NotNil(t, got)

	buf, err = structs.Encode(structs.BackupDeleteRequestType|structs.IgnoreUnknownTypeFlag,
		&structs.BackupDeleteRequest{IDs: []string{record.ID}})
	must.NoError(t, err)
	must.Nil(t, fsm.Apply(makeLog(buf)))
	got, err = fsm.State().BackupByID(nil, record.ID)
	must.NoError(t, err)
	must.Nil(t, got)
}

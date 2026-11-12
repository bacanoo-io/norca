// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/backup/s3fake"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
)

// TestBackup_TakenByObserver checks backup prefer_observer with a leader and
// an observer: a manual backup is taken by the observer and recorded by the
// leader; the checks of Backup.TakeForLeader; with the observer gone the
// leader takes the backup itself.
func TestBackup_TakenByObserver(t *testing.T) {
	ci.Parallel(t)

	s3 := s3fake.New()
	defer s3.Close()
	identity, err := age.GenerateX25519Identity()
	must.NoError(t, err)

	s1, cleanup1 := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
		c.DevMode = false
		c.DataDir = t.TempDir()
		c.Backup = testBackupConfig(s3, identity, false)
		c.Backup.PreferObserver = true
	})
	defer cleanup1()
	testutil.WaitForLeader(t, s1.RPC)

	s2, cleanup2 := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
		c.DevMode = false
		c.DataDir = t.TempDir()
		c.BootstrapExpect = 0
		c.Role = ServerRoleObserver
		c.Backup = testBackupConfig(s3, identity, false)
		c.Backup.PreferObserver = true
	})
	defer cleanup2()
	TestJoin(t, s1, s2)

	// The observer is a candidate once autopilot sees it healthy and its
	// keyring has the active key.
	testutil.WaitForResultUntil(30*time.Second, func() (bool, error) {
		if len(s1.observerCandidates()) != 1 {
			return false, fmt.Errorf("no observer candidate yet")
		}
		if _, err := s2.backupKeyring(); err != nil {
			return false, err
		}
		// Raft takes no snapshot before the configuration entry of the
		// join is applied.
		for _, srv := range []*Server{s1, s2} {
			future := srv.raft.GetConfiguration()
			if err := future.Error(); err != nil {
				return false, err
			}
			if applied := srv.raft.AppliedIndex(); applied < future.Index() {
				return false, fmt.Errorf("%s applied %d of configuration %d", srv.config.NodeName, applied, future.Index())
			}
		}
		return true, nil
	}, func(err error) { must.NoError(t, err) })

	// Raft takes a snapshot at the last index applied to the state; the
	// configuration entry of the join is not one. A write after it, applied
	// on both servers, makes snapshots possible.
	var nsReply structs.GenericResponse
	must.NoError(t, s1.RPC("Namespace.UpsertNamespaces", &structs.NamespaceUpsertRequest{
		Namespaces:   []*structs.Namespace{mock.Namespace()},
		WriteRequest: structs.WriteRequest{Region: s1.Region()},
	}, &nsReply))
	testutil.WaitForResultUntil(30*time.Second, func() (bool, error) {
		if applied := s2.raft.AppliedIndex(); applied < nsReply.Index {
			return false, fmt.Errorf("observer applied %d of %d", applied, nsReply.Index)
		}
		return true, nil
	}, func(err error) { must.NoError(t, err) })

	save := func() *structs.BackupRecord {
		t.Helper()
		var reply structs.BackupSaveResponse
		must.NoError(t, s1.RPC("Backup.Save", &structs.BackupSaveRequest{
			WriteRequest: structs.WriteRequest{Region: s1.Region()},
		}, &reply))
		must.NotNil(t, reply.Backup)
		must.Eq(t, structs.BackupStatusComplete, reply.Backup.Status, must.Sprint(reply.Backup.StatusDescription))
		return reply.Backup
	}

	record := save()
	must.Eq(t, s2.config.NodeName, record.TakenBy)
	must.Eq(t, 1, s1.metricsCounters.backupsByObserver.Load())
	stored, err := s1.State().BackupByID(nil, record.ID)
	must.NoError(t, err)
	must.Eq(t, s2.config.NodeName, stored.TakenBy)
	must.Len(t, 1, stored.KeyIDs)

	// The checks of the observer side.
	_, leaderID := s1.raft.LeaderWithID()
	take := func(srv *Server, leader string) *structs.BackupTakeForLeaderResponse {
		t.Helper()
		var reply structs.BackupTakeForLeaderResponse
		must.NoError(t, srv.RPC("Backup.TakeForLeader", &structs.BackupTakeForLeaderRequest{
			LeaderID:     leader,
			ID:           "20261111T120000Z-1",
			Fingerprint:  s1.backup.Config().Fingerprint(),
			WriteRequest: structs.WriteRequest{Region: s1.Region()},
		}, &reply))
		return &reply
	}
	must.StrContains(t, take(s1, string(leaderID)).Refused, `has the role "voter"`)
	must.StrContains(t, take(s2, "someone-else").Refused, "is not the current leader")

	// Without the observer the leader takes the backup.
	must.NoError(t, s2.Shutdown())
	testutil.WaitForResultUntil(30*time.Second, func() (bool, error) {
		if n := len(s1.observerCandidates()); n != 0 {
			return false, fmt.Errorf("%d observer candidates", n)
		}
		return true, nil
	}, func(err error) { must.NoError(t, err) })
	record = save()
	must.Eq(t, s1.config.NodeName, record.TakenBy)
	must.Eq(t, 1, s1.metricsCounters.backupsByObserver.Load())
}

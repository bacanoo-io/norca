// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"
	"net/http/httptest"
	"path"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/backup/s3fake"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

func TestHTTP_Backups(t *testing.T) {
	ci.Parallel(t)

	s3 := s3fake.New()
	defer s3.Close()
	identity, err := age.GenerateX25519Identity()
	must.NoError(t, err)
	dir := t.TempDir()

	httpTest(t, func(c *Config) {
		c.DevMode = false
		c.DataDir = path.Join(dir, "server")
		c.Client.Enabled = false
		c.AdvertiseAddrs.HTTP = "127.0.0.1"
		c.AdvertiseAddrs.RPC = "127.0.0.1"
		c.AdvertiseAddrs.Serf = "127.0.0.1"
		c.Server.Backup = &BackupConfig{
			Destinations: []*BackupDestinationConfig{{
				Name:           "primary",
				Type:           "s3",
				Endpoint:       s3.Endpoint(),
				Bucket:         "backups",
				Region:         "us-east-1",
				ForcePathStyle: pointer.Of(true),
				AccessKey:      "test",
				SecretKey:      "testsecret",
			}},
			Encryption: &BackupEncryptionConfig{Recipients: []string{identity.Recipient().String()}},
		}
	}, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.RPC)
		client := s.Client()
		backups := client.Backups()

		// The keyring is initialized shortly after the leader election.
		var saved *api.BackupSaveResponse
		must.Wait(t, wait.InitialSuccess(
			wait.ErrorFunc(func() error {
				var err error
				saved, _, err = backups.Save(&api.BackupSaveRequest{Labels: map[string]string{"note": "http"}}, nil)
				return err
			}),
			wait.Timeout(10*time.Second),
			wait.Gap(200*time.Millisecond),
		))
		must.Eq(t, api.BackupStatusComplete, saved.Backup.Status)

		list, _, err := backups.List(nil)
		must.NoError(t, err)
		must.Len(t, 1, list.Backups)
		must.Eq(t, saved.ID, list.Backups[0].ID)
		must.False(t, list.Schedule.Enabled)
		must.Eq(t, []string{"primary"}, list.Schedule.Destinations)

		info, _, err := backups.Info(saved.ID, nil)
		must.NoError(t, err)
		must.Eq(t, "http", info.Labels["note"])
		_, _, err = backups.Info("20200101T000000Z-1", nil)
		must.ErrorContains(t, err, "404")

		// Browse with the identity in the body.
		contents, _, err := backups.Contents(saved.ID, &api.BackupContentsOptions{
			Type: api.BackupContentNamespaces, AgeIdentity: identity.String(),
		}, nil)
		must.NoError(t, err)
		must.Eq(t, api.BackupContentNamespaces, contents.Type)
		must.SliceNotEmpty(t, contents.Namespaces)

		// And with the identity in a header of a GET.
		req, err := http.NewRequest(http.MethodGet, "/v1/operator/backup/"+saved.ID+"/contents?type=namespaces", nil)
		must.NoError(t, err)
		req.Header.Set(backupIdentityHeader, identity.String())
		resp := httptest.NewRecorder()
		obj, err := s.Server.BackupSpecificRequest(resp, req)
		must.NoError(t, err)
		must.SliceNotEmpty(t, obj.(*structs.BackupContents).Namespaces)

		diff, _, err := backups.Diff(saved.ID, &api.BackupDiffOptions{AgeIdentity: identity.String()}, nil)
		must.NoError(t, err)
		must.Eq(t, "current", diff.To)

		// Restore preview of a namespace.
		restore, _, err := backups.Restore(saved.ID, &api.BackupRestoreRequest{
			Items:       []*api.BackupRestoreItem{{Type: api.BackupRestoreItemNamespace, ID: "default"}},
			DryRun:      true,
			AgeIdentity: identity.String(),
		}, nil)
		must.NoError(t, err)
		must.Len(t, 1, restore.Results)
		must.Eq(t, "update", restore.Results[0].Action)

		// Pin, then deletion is refused.
		updated, _, err := backups.Update(saved.ID, &api.BackupUpdateRequest{Pinned: pointer.Of(true)}, nil)
		must.NoError(t, err)
		must.True(t, updated.Pinned)
		_, err = backups.Delete(saved.ID, nil)
		must.ErrorContains(t, err, "pinned")

		url, _, err := backups.DownloadURL(saved.ID, "", api.BackupObjectManifest, nil)
		must.NoError(t, err)
		must.StrContains(t, url.URL, "manifest.json")

		// The restore records: the selective dry run above recorded none.
		restores, _, err := backups.ListRestores(nil)
		must.NoError(t, err)
		must.Len(t, 0, restores)
		_, _, err = backups.GetRestore("abc", nil)
		must.ErrorContains(t, err, "404")

		// The two-person approval of full restores needs ACLs.
		_, _, err = backups.ApproveRestore("abc", &api.BackupRestoreApproveRequest{AgeIdentity: identity.String()}, nil)
		must.ErrorContains(t, err, "ACLs enabled")
		_, _, err = backups.Restore(saved.ID, &api.BackupRestoreRequest{
			Mode: api.BackupRestoreModeFull, DryRun: true, AgeIdentity: identity.String(),
		}, nil)
		must.ErrorContains(t, err, "ACLs enabled")
		req, err = http.NewRequest(http.MethodPost, "/v1/operator/backup/restore/abc/unknown", nil)
		must.NoError(t, err)
		_, err = s.Server.BackupSpecificRequest(httptest.NewRecorder(), req)
		must.ErrorContains(t, err, "unknown restore resource")

		// Root keys of a backup are added to the keystore.
		key, err := structs.NewRootKey(structs.EncryptionAlgorithmAES256GCM)
		must.NoError(t, err)
		added, _, err := client.Keyring().Restore([]*api.RootKey{{
			Meta: &api.RootKeyMeta{KeyID: key.Meta.KeyID, Algorithm: api.EncryptionAlgorithmAES256GCM,
				CreateTime: key.Meta.CreateTime, State: api.RootKeyStateInactive},
			Key: key.Key,
		}}, nil)
		must.NoError(t, err)
		must.Eq(t, []string{key.Meta.KeyID}, added.Added)
	})
}

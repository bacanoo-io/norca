// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package backup

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/backup/s3fake"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

type memSnapshot struct {
	data  []byte
	index uint64
}

func (s *memSnapshot) Index() uint64                 { return s.index }
func (s *memSnapshot) Term() uint64                  { return 3 }
func (s *memSnapshot) NewReader() (io.Reader, error) { return bytes.NewReader(s.data), nil }
func (s *memSnapshot) Close() error                  { return nil }

type testEnv struct {
	store    *state.StateStore
	identity *age.X25519Identity
	s3       *s3fake.Server
	snapshot []byte
	keys     []*structs.RootKey
	index    atomic.Uint64
	deps     *Deps
}

func newTestEnv(t *testing.T, snapshotSize int) *testEnv {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	must.NoError(t, err)

	env := &testEnv{
		store:    state.TestStateStore(t),
		identity: identity,
		s3:       s3fake.New(),
		snapshot: make([]byte, snapshotSize),
	}
	t.Cleanup(env.s3.Close)
	_, err = rand.Read(env.snapshot)
	must.NoError(t, err)
	env.index.Store(1000)

	key, err := structs.NewRootKey(structs.EncryptionAlgorithmAES256GCM)
	must.NoError(t, err)
	env.keys = []*structs.RootKey{key}

	env.deps = &Deps{
		Region:              "eu",
		AuthoritativeRegion: "eu",
		LeaderID:            "srv-1",
		ServersReady:        func() bool { return true },
		Snapshot: func() (Snapshot, error) {
			return &memSnapshot{data: env.snapshot, index: env.index.Add(1)}, nil
		},
		Keyring: func() ([]*structs.RootKey, error) { return env.keys, nil },
		State:   func() *state.StateStore { return env.store },
		Apply: func(msgType structs.MessageType, req any) (uint64, error) {
			index := env.index.Add(1)
			switch msgType &^ structs.IgnoreUnknownTypeFlag {
			case structs.BackupUpsertRequestType:
				return index, env.store.UpsertBackups(msgType, index, req.(*structs.BackupUpsertRequest).Backups)
			case structs.BackupDeleteRequestType:
				return index, env.store.DeleteBackups(msgType, index, req.(*structs.BackupDeleteRequest).IDs)
			}
			t.Fatalf("unexpected message type %d", msgType)
			return 0, nil
		},
	}
	return env
}

func (env *testEnv) config(names ...string) *Config {
	cfg := DefaultConfig()
	cfg.PartSize = minPartSize
	cfg.UploadConcurrency = 2
	cfg.VerifyChecksum = true
	cfg.Encryption.Recipients = []string{env.identity.Recipient().String()}
	for _, name := range names {
		cfg.Destinations = append(cfg.Destinations, &DestinationConfig{
			Name:           name,
			Type:           DestinationTypeS3,
			Endpoint:       env.s3.Endpoint(),
			Bucket:         "bucket-" + name,
			Prefix:         "norca/${region}",
			Region:         "us-east-1",
			ForcePathStyle: true,
			AccessKey:      "test",
			SecretKey:      "testsecret",
			Prune:          true,
		})
	}
	return cfg
}

func (env *testEnv) identities(t *testing.T) Identities {
	ids, err := ParseIdentities("# test\n" + env.identity.String() + "\n")
	must.NoError(t, err)
	return ids
}

func TestManager_SaveOpen(t *testing.T) {
	ci.Parallel(t)
	// Larger than two parts, to cover the multipart upload.
	env := newTestEnv(t, 2*minPartSize+12345)
	m := New(env.config("primary", "dr"), env.deps)
	m.SetLeader(true)
	defer m.SetLeader(false)

	var started string
	record, err := m.Save(SaveOptions{
		Labels:  map[string]string{"note": "before upgrade"},
		Pin:     true,
		Started: func(id string) { started = id },
	})
	must.NoError(t, err)
	must.Eq(t, started, record.ID)
	must.Eq(t, structs.BackupStatusComplete, record.Status)
	must.Eq(t, structs.BackupTriggerManual, record.Trigger)
	must.True(t, record.Pinned)
	must.Len(t, 2, record.Destinations)
	must.Eq(t, []string{env.keys[0].Meta.KeyID}, record.KeyIDs)
	must.StrHasPrefix(t, "s3://bucket-primary/norca/eu/", record.Destinations[0].Location)

	// The record is in the state store.
	stored, err := env.store.BackupByID(nil, record.ID)
	must.NoError(t, err)
	must.NotNil(t, stored)

	// Object layout.
	folder := "norca/eu/" + record.Path + "/"
	must.Eq(t, []string{
		folder + KeyringKey, folder + ManifestKey, folder + StateKey, "norca/eu/" + LatestKey,
	}, env.s3.Keys("bucket-primary"))
	must.Zero(t, env.s3.IncompleteUploads())

	var manifest Manifest
	must.NoError(t, json.Unmarshal(env.s3.Object("bucket-dr", folder+ManifestKey), &manifest))
	must.Eq(t, record.ID, manifest.ID)
	must.Eq(t, "eu", manifest.Region)
	must.Eq(t, "dr", manifest.Destination)
	must.Eq(t, EncryptionModeAge, manifest.Encryption.Mode)
	must.True(t, manifest.Pinned)
	must.Eq(t, record.KeyIDs, manifest.Objects[structs.BackupObjectKeyring].KeyIDs)

	// The stored state is encrypted.
	must.False(t, bytes.Contains(env.s3.Object("bucket-primary", folder+StateKey), env.snapshot[:64]))

	// Decrypt the state from the secondary destination.
	rc, err := m.Open(t.Context(), record, "dr", structs.BackupObjectState, env.identities(t))
	must.NoError(t, err)
	got, err := io.ReadAll(rc)
	must.NoError(t, err)
	must.NoError(t, rc.Close())
	must.True(t, bytes.Equal(env.snapshot, got))

	// And the keyring.
	rc, err = m.Open(t.Context(), record, "", structs.BackupObjectKeyring, env.identities(t))
	must.NoError(t, err)
	keys, err := DecodeKeyring(rc)
	must.NoError(t, err)
	rc.Close()
	must.Len(t, 1, keys)
	must.Eq(t, env.keys[0].Key, keys[0].Key)

	// A different identity can't decrypt.
	other, err := age.GenerateX25519Identity()
	must.NoError(t, err)
	ids, err := ParseIdentities(other.String())
	must.NoError(t, err)
	_, err = m.Open(t.Context(), record, "", structs.BackupObjectState, ids)
	must.ErrorContains(t, err, "does not match")

	// A modified object is detected.
	tampered := env.s3.Object("bucket-primary", folder+StateKey)
	tampered[len(tampered)-10] ^= 0xff
	env.s3.PutObject("bucket-primary", folder+StateKey, tampered)
	rc, err = m.Open(t.Context(), record, "primary", structs.BackupObjectState, env.identities(t))
	must.NoError(t, err)
	_, err = io.ReadAll(rc)
	must.Error(t, err)
	rc.Close()

	// The record can be rebuilt from the manifest.
	loaded, err := m.LoadRecord(t.Context(), record.ID, "dr")
	must.NoError(t, err)
	must.Eq(t, record.RaftIndex, loaded.RaftIndex)
	must.Eq(t, record.Destinations[1].Objects[structs.BackupObjectState].SHA256,
		loaded.Destinations[0].Objects[structs.BackupObjectState].SHA256)

	// A pre-signed URL can be created.
	u, err := m.PresignURL(t.Context(), record, "", structs.BackupObjectState, time.Minute)
	must.NoError(t, err)
	must.StrContains(t, u, "X-Amz-Signature")
}

func TestManager_PartialAndFailed(t *testing.T) {
	ci.Parallel(t)
	env := newTestEnv(t, 4096)
	failing := s3fake.New()
	defer failing.Close()
	failing.SetFailing(true)

	cfg := env.config("primary", "dr")
	cfg.Destinations[1].Endpoint = failing.Endpoint()
	m := New(cfg, env.deps)
	m.SetLeader(true)
	defer m.SetLeader(false)

	record, err := m.Save(SaveOptions{})
	must.NoError(t, err)
	must.Eq(t, structs.BackupStatusPartial, record.Status)
	must.Eq(t, []string{"primary"}, record.VerifiedDestinations())
	must.Eq(t, structs.BackupDestinationStatusFailed, record.Destination("dr").Status)
	must.StrContains(t, record.StatusDescription, "dr:")

	// All destinations failing: a failed record without objects.
	cfg.Destinations[0].Endpoint = failing.Endpoint()
	must.NoError(t, m.SetConfig(cfg))
	record, err = m.Save(SaveOptions{})
	must.Error(t, err)
	must.Eq(t, structs.BackupStatusFailed, record.Status)
	stored, err := env.store.BackupByID(nil, record.ID)
	must.NoError(t, err)
	must.Eq(t, structs.BackupStatusFailed, stored.Status)
}

func TestManager_KMSNotImplemented(t *testing.T) {
	ci.Parallel(t)
	env := newTestEnv(t, 16)
	cfg := env.config("primary")
	cfg.Encryption = &EncryptionConfig{Mode: EncryptionModeKMS, KMSKey: "arn:aws:kms:x"}
	must.ErrorContains(t, cfg.Validate(), `"kms" is not implemented`)
}

func TestManager_UpdateDelete(t *testing.T) {
	ci.Parallel(t)
	env := newTestEnv(t, 1024)
	cfg := env.config("primary", "dr")
	cfg.Destinations[1].Prune = false
	cfg.Retention = &RetentionConfig{MinKeep: 1}
	m := New(cfg, env.deps)
	m.SetLeader(true)
	defer m.SetLeader(false)

	first, err := m.Save(SaveOptions{})
	must.NoError(t, err)

	// Pin and label.
	updated := first.Copy()
	updated.Pinned = true
	updated.Labels = map[string]string{"note": "keep"}
	_, err = m.Update(updated)
	must.NoError(t, err)
	var manifest Manifest
	must.NoError(t, json.Unmarshal(env.s3.Object("bucket-primary", "norca/eu/"+first.Path+"/"+ManifestKey), &manifest))
	must.True(t, manifest.Pinned)
	must.Eq(t, "keep", manifest.Labels["note"])

	// The pinned backup survives retention; with min_keep 1 and no other
	// keep rule, the second backup expires once a third exists.
	second, err := m.Save(SaveOptions{})
	must.NoError(t, err)
	_, err = m.Save(SaveOptions{})
	must.NoError(t, err)

	records, err := env.store.BackupsList(nil)
	must.NoError(t, err)
	ids := []string{}
	for _, r := range records {
		ids = append(ids, r.ID)
	}
	must.SliceContains(t, ids, first.ID)
	must.SliceNotContains(t, ids, second.ID)

	// The expired backup is gone from the pruned destination only.
	for _, k := range env.s3.Keys("bucket-primary") {
		must.StrNotContains(t, k, second.ID)
	}
	found := false
	for _, k := range env.s3.Keys("bucket-dr") {
		found = found || strings.Contains(k, second.ID)
	}
	must.True(t, found)

	// Manual deletion.
	stored, err := env.store.BackupByID(nil, first.ID)
	must.NoError(t, err)
	_, err = m.Delete(stored)
	must.NoError(t, err)
	stored, err = env.store.BackupByID(nil, first.ID)
	must.NoError(t, err)
	must.Nil(t, stored)
}

func TestManager_Scheduler(t *testing.T) {
	ci.Parallel(t)
	env := newTestEnv(t, 2048)
	cfg := env.config("primary")
	cfg.Enabled = true

	// An upload interrupted by a previous leader.
	env.s3.StartUpload("bucket-primary", "norca/eu/2026/01/01/20260101T000000Z-1/state.snap.age")

	m := New(cfg, env.deps)
	m.SetLeader(true)
	defer m.SetLeader(false)

	// Without any scheduled backup the first one is due immediately.
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			records, _ := env.store.BackupsList(nil)
			return len(records) == 1 && records[0].Trigger == structs.BackupTriggerSchedule
		}),
		wait.Timeout(20*time.Second),
		wait.Gap(100*time.Millisecond),
	))
	must.Zero(t, env.s3.IncompleteUploads())

	records, err := env.store.BackupsList(nil)
	must.NoError(t, err)
	status := cfg.ScheduleStatus(records, "eu", time.Now())
	must.True(t, status.Enabled)
	must.Eq(t, records[0].ID, status.LastSuccessID)
	must.True(t, status.NextDue.After(time.Now()))
	must.False(t, status.Overdue)
}

func TestKeyringEncoding(t *testing.T) {
	ci.Parallel(t)
	key, err := structs.NewRootKey(structs.EncryptionAlgorithmAES256GCM)
	must.NoError(t, err)
	buf, err := EncodeKeyring([]*structs.RootKey{key})
	must.NoError(t, err)
	keys, err := DecodeKeyring(bytes.NewReader(buf))
	must.NoError(t, err)
	must.Eq(t, key.Meta.KeyID, keys[0].Meta.KeyID)
	must.Eq(t, key.Key, keys[0].Key)
}

func TestEncryptor_RoundTrip(t *testing.T) {
	ci.Parallel(t)
	id1, _ := age.GenerateX25519Identity()
	id2, _ := age.GenerateX25519Identity()
	keys := []string{id1.Recipient().String(), id2.Recipient().String()}
	enc, err := NewEncryptor(&EncryptionConfig{Recipients: keys})
	must.NoError(t, err)
	must.Eq(t, RecipientsFingerprint(keys), enc.Fingerprint())
	must.Eq(t, RecipientsFingerprint([]string{keys[1], keys[0]}), enc.Fingerprint())

	var buf bytes.Buffer
	w, err := enc.Encrypt(&buf)
	must.NoError(t, err)
	_, err = w.Write([]byte("secret state"))
	must.NoError(t, err)
	must.NoError(t, w.Close())

	// Either recipient's identity decrypts.
	for _, id := range []*age.X25519Identity{id1, id2} {
		ids, err := ParseIdentities(id.String())
		must.NoError(t, err)
		r, err := ids.decrypt(bytes.NewReader(buf.Bytes()))
		must.NoError(t, err)
		out, err := io.ReadAll(r)
		must.NoError(t, err)
		must.Eq(t, "secret state", string(out))
	}

	_, err = ParseIdentities("")
	must.ErrorContains(t, err, "identity is required")
	_, err = ParseIdentities("AGE-SECRET-KEY-1NOTAKEY")
	must.Error(t, err)
	must.StrNotContains(t, err.Error(), "NOTAKEY")
	_, err = NewEncryptor(&EncryptionConfig{Recipients: []string{"age1bogus"}})
	must.Error(t, err)
}

func TestPathAndID(t *testing.T) {
	ci.Parallel(t)
	ts := time.Date(2026, 10, 24, 14, 0, 0, 0, time.UTC)
	id := NewID(ts, 1834567)
	must.Eq(t, "20261024T140000Z-1834567", id)
	p, err := PathFor(id)
	must.NoError(t, err)
	must.Eq(t, "2026/10/24/20261024T140000Z-1834567", p)
	parsed, index, err := ParseID(id)
	must.NoError(t, err)
	must.Eq(t, ts, parsed)
	must.Eq(t, uint64(1834567), index)
	for _, bad := range []string{"", "x", "20261024T140000Z", "../../x-1", "20261024T140000Z-x"} {
		_, err := PathFor(bad)
		must.Error(t, err, must.Sprint(bad))
	}
}

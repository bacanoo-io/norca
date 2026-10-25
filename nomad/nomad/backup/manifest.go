// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package backup

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/nomad/nomad/structs"
)

// Object keys inside a backup folder, and the pointer to the latest backup.
const (
	ManifestKey = "manifest.json"
	StateKey    = "state.snap.age"
	KeyringKey  = "keyring.age"
	LatestKey   = "latest.json"

	// SecretStoreKeyKey is the key of the secret store core (Norca), next
	// to the keyring and encrypted the same way.
	SecretStoreKeyKey = "secret-store-key.age"

	// idTimeFormat is the time part of a backup ID.
	idTimeFormat = "20060102T150405Z"
)

// ObjectKey returns the key of a backup object, relative to the backup
// folder.
func ObjectKey(object string) (string, error) {
	switch object {
	case structs.BackupObjectState:
		return StateKey, nil
	case structs.BackupObjectKeyring:
		return KeyringKey, nil
	case structs.BackupObjectSecretStoreKey:
		return SecretStoreKeyKey, nil
	case structs.BackupObjectManifest:
		return ManifestKey, nil
	}
	return "", fmt.Errorf("unknown backup object %q", object)
}

// NewID returns the ID of a backup taken at t of the Raft index.
func NewID(t time.Time, index uint64) string {
	return t.UTC().Format(idTimeFormat) + "-" + strconv.FormatUint(index, 10)
}

// ParseID returns the time and the Raft index of a backup ID.
func ParseID(id string) (time.Time, uint64, error) {
	ts, idx, ok := strings.Cut(id, "-")
	if !ok {
		return time.Time{}, 0, fmt.Errorf("invalid backup ID %q", id)
	}
	t, err := time.Parse(idTimeFormat, ts)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("invalid backup ID %q", id)
	}
	index, err := strconv.ParseUint(idx, 10, 64)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("invalid backup ID %q", id)
	}
	return t, index, nil
}

// PathFor returns the folder of a backup relative to the destination
// prefix: YYYY/MM/DD/<id>.
func PathFor(id string) (string, error) {
	t, _, err := ParseID(id)
	if err != nil {
		return "", err
	}
	return path.Join(t.Format("2006/01/02"), id), nil
}

// Manifest is the unencrypted description of a backup on a destination. It
// holds counts only, never names or content, so backups can be listed
// without decrypting them.
type Manifest struct {
	ID                  string                     `json:"id"`
	Region              string                     `json:"region"`
	AuthoritativeRegion string                     `json:"authoritative_region"`
	StartedAt           time.Time                  `json:"started_at"`
	CreatedAt           time.Time                  `json:"created_at"`
	Trigger             string                     `json:"trigger"`
	NorcaVersion        string                     `json:"norca_version"`
	NomadVersion        string                     `json:"nomad_version"`
	RaftIndex           uint64                     `json:"raft_index"`
	RaftTerm            uint64                     `json:"raft_term"`
	LeaderID            string                     `json:"leader_id"`
	Destination         string                     `json:"destination"`
	Objects             map[string]*ManifestObject `json:"objects"`
	Encryption          ManifestEncryption         `json:"encryption"`
	Summary             map[string]int             `json:"summary"`
	Labels              map[string]string          `json:"labels,omitempty"`
	Pinned              bool                       `json:"pinned"`

	// Seals are the keyring seals that wrapped the keys of the region when
	// the backup was taken (Norca).
	Seals []*ManifestSeal `json:"seals,omitempty"`
}

// ManifestSeal is a keyring seal of the region of a backup. It never holds
// a key.
type ManifestSeal struct {
	Type      string `json:"type"`
	KeyID     string `json:"key_id,omitempty"`
	Threshold int    `json:"threshold,omitempty"`
	Shares    int    `json:"shares,omitempty"`
}

// ManifestObject is an encrypted object of the backup.
type ManifestObject struct {
	Key    string   `json:"key"`
	Size   int64    `json:"size"`
	SHA256 string   `json:"sha256"`
	KeyIDs []string `json:"key_ids,omitempty"`
}

// ManifestEncryption describes the client-side encryption.
type ManifestEncryption struct {
	Mode                  string `json:"mode"`
	RecipientsFingerprint string `json:"recipients_fingerprint"`
}

// Latest is the content of latest.json.
type Latest struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"created_at"`
}

// NewManifest returns the manifest of a backup on a destination.
func NewManifest(b *structs.BackupRecord, d *structs.BackupDestinationStatus) *Manifest {
	m := &Manifest{
		ID:                  b.ID,
		Region:              b.Region,
		AuthoritativeRegion: b.AuthoritativeRegion,
		StartedAt:           b.StartedAt.UTC(),
		CreatedAt:           b.CompletedAt.UTC(),
		Trigger:             b.Trigger,
		NorcaVersion:        b.NorcaVersion,
		NomadVersion:        b.NomadVersion,
		RaftIndex:           b.RaftIndex,
		RaftTerm:            b.RaftTerm,
		LeaderID:            b.LeaderID,
		Destination:         d.Name,
		Objects:             make(map[string]*ManifestObject, len(d.Objects)),
		Encryption: ManifestEncryption{
			Mode:                  d.EncryptionMode,
			RecipientsFingerprint: d.RecipientsFingerprint,
		},
		Summary: maps.Clone(b.Summary),
		Labels:  maps.Clone(b.Labels),
		Pinned:  b.Pinned,
	}
	for _, s := range b.Seals {
		m.Seals = append(m.Seals, &ManifestSeal{Type: s.Type, KeyID: s.KeyID, Threshold: s.Threshold, Shares: s.Shares})
	}
	for name, o := range d.Objects {
		mo := &ManifestObject{Key: o.Key, Size: o.Size, SHA256: o.SHA256}
		if name == structs.BackupObjectKeyring {
			mo.KeyIDs = slices.Clone(b.KeyIDs)
		}
		m.Objects[name] = mo
	}
	return m
}

// Record returns a backup record built from the manifest found on a
// destination, for backups that are not (or no longer) in the
// backup_status table.
func (m *Manifest) Record(dest Destination, backupPath string) *structs.BackupRecord {
	ds := &structs.BackupDestinationStatus{
		Name:                  dest.Name(),
		Status:                structs.BackupDestinationStatusVerified,
		Location:              dest.Location(backupPath + "/"),
		EncryptionMode:        m.Encryption.Mode,
		RecipientsFingerprint: m.Encryption.RecipientsFingerprint,
		Objects:               make(map[string]*structs.BackupObject, len(m.Objects)),
	}
	var keyIDs []string
	for name, o := range m.Objects {
		ds.Objects[name] = &structs.BackupObject{Key: o.Key, Size: o.Size, SHA256: o.SHA256}
		if name == structs.BackupObjectKeyring {
			keyIDs = slices.Clone(o.KeyIDs)
		}
	}
	var seals []*structs.SealInfo
	for _, s := range m.Seals {
		seals = append(seals, &structs.SealInfo{Type: s.Type, KeyID: s.KeyID, Threshold: s.Threshold, Shares: s.Shares})
	}
	return &structs.BackupRecord{
		Seals:               seals,
		ID:                  m.ID,
		Region:              m.Region,
		AuthoritativeRegion: m.AuthoritativeRegion,
		Trigger:             m.Trigger,
		Status:              structs.BackupStatusComplete,
		StartedAt:           m.StartedAt,
		CompletedAt:         m.CreatedAt,
		NorcaVersion:        m.NorcaVersion,
		NomadVersion:        m.NomadVersion,
		RaftIndex:           m.RaftIndex,
		RaftTerm:            m.RaftTerm,
		LeaderID:            m.LeaderID,
		Path:                backupPath,
		Destinations:        []*structs.BackupDestinationStatus{ds},
		KeyIDs:              keyIDs,
		Summary:             maps.Clone(m.Summary),
		Labels:              maps.Clone(m.Labels),
		Pinned:              m.Pinned,
	}
}

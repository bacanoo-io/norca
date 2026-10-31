// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package backup

import (
	"context"
	"errors"
	"fmt"
	"io"

	goversion "github.com/hashicorp/go-version"
	"github.com/hashicorp/nomad/helper/consulkv"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/version"
)

// CheckRestorable returns an error if the Raft state of a backup must not
// be restored into a region: the backup belongs to another region (unless
// allowRegionChange is set; the region references of the jobs are not
// rewritten), or it was taken by a newer Norca version than this one.
func CheckRestorable(record *structs.BackupRecord, region string, allowRegionChange bool) error {
	if record.Region != "" && record.Region != region && !allowRegionChange {
		return fmt.Errorf("backup %s is of region %q, not %q; the region references of its jobs are not rewritten",
			record.ID, record.Region, region)
	}
	if record.NorcaVersion == "" {
		return nil
	}
	backupVersion, err := goversion.NewVersion(record.NorcaVersion)
	if err != nil {
		return fmt.Errorf("backup %s has an invalid Norca version %q", record.ID, record.NorcaVersion)
	}
	current := goversion.Must(goversion.NewVersion(version.NorcaVersion))
	if backupVersion.Core().GreaterThan(current.Core()) {
		return fmt.Errorf("backup %s was taken by Norca %s and can't be restored by the older Norca %s",
			record.ID, record.NorcaVersion, version.NorcaVersion)
	}
	return nil
}

// DownloadState decrypts the state object (the Raft snapshot archive) of a
// backup into w. The checksum of the stored object is verified before it
// returns, so a modified or truncated backup is an error, not a partial
// archive.
func DownloadState(ctx context.Context, dest Destination, record *structs.BackupRecord,
	st *structs.BackupDestinationStatus, ids Identities, w io.Writer) (int64, error) {

	rc, err := OpenObject(ctx, dest, record, st, structs.BackupObjectState, ids)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	n, err := io.Copy(w, rc)
	if err != nil {
		return n, fmt.Errorf("failed to download the state of backup %s: %w", record.ID, err)
	}
	return n, nil
}

// ReadKeys decrypts the keyring object of a backup. It returns nil keys
// and no error if the backup has no keyring.
func ReadKeys(ctx context.Context, dest Destination, record *structs.BackupRecord,
	st *structs.BackupDestinationStatus, ids Identities) ([]*structs.RootKey, error) {

	rc, err := OpenObject(ctx, dest, record, st, structs.BackupObjectKeyring, ids)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	keys, err := DecodeKeyring(rc)
	if err == nil {
		// Read to the end, so the checksum of the object is verified.
		_, err = io.Copy(io.Discard, rc)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read the keyring of backup %s: %w", record.ID, err)
	}
	return keys, nil
}

// ReadSecretStoreKey decrypts the secret store key object of a backup (the
// key of its secret store core). It returns nil and no error if the backup
// has none: backups taken before Norca wrote it, or without the keyring.
func ReadSecretStoreKey(ctx context.Context, dest Destination, record *structs.BackupRecord,
	st *structs.BackupDestinationStatus, ids Identities) ([]byte, error) {

	rc, err := OpenObject(ctx, dest, record, st, structs.BackupObjectSecretStoreKey, ids)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	key, err := DecodeSecretStoreKey(rc)
	if err != nil {
		return nil, fmt.Errorf("failed to read the secret store key of backup %s: %w", record.ID, err)
	}
	return key, nil
}

// ReadConsulKV reads the export of the Consul KV bucket of a backup; nil
// when the backup has none.
func ReadConsulKV(ctx context.Context, dest Destination, record *structs.BackupRecord,
	st *structs.BackupDestinationStatus, ids Identities) (*consulkv.Export, error) {

	rc, err := OpenObject(ctx, dest, record, st, structs.BackupObjectConsulKV, ids)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	export, err := consulkv.Decode(rc)
	if err != nil {
		return nil, fmt.Errorf("failed to read the Consul KV of backup %s: %w", record.ID, err)
	}
	return export, nil
}

// CheckIdentity verifies that the identities can decrypt a backup on a
// destination, by reading the header of its state object only.
func CheckIdentity(ctx context.Context, dest Destination, record *structs.BackupRecord,
	st *structs.BackupDestinationStatus, ids Identities) error {

	rc, err := OpenObject(ctx, dest, record, st, structs.BackupObjectState, ids)
	if err != nil {
		return err
	}
	return rc.Close()
}

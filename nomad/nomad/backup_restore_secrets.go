// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/secretstore"
	"github.com/hashicorp/nomad/secretstore/ubix/export/barrier"
	"github.com/hashicorp/nomad/secretstore/ubix/export/storage"
)

// Selective restore of a secret. A backup holds a secret as its metadata
// row and the barrier's ciphertext blobs of the KV engine. Restoring a
// secret takes one version of it from the backup (by default the version
// that was current at backup time), decrypts its items through the
// leader's core, and writes them through the normal write path as a new
// version on top of the secret's current history: the version numbers only
// grow, the existing versions stay (max_versions ages out the oldest as with
// any write), a soft deleted secret becomes live again, and a secret that no
// longer exists is created. The write is a normal secret write (the
// SecretApplyRequestType entry, the Secret event) with its own audit record,
// which names the backup and the version the value came from.
//
// By default only the value is restored and the current metadata is kept;
// with WithMetadata the backup's metadata replaces it. A secret that does
// not exist gets the backup's metadata either way.
//
// The check index of the item is an optional guard: without it the value is
// always written as a new version; 0 requires that the secret does not
// exist, any other value that it is the secret's current modify index.

// secretKVDataPrefix is the barrier prefix of the version data of a secret,
// as the KV engine stores it ("<prefix>/data/<key>/<version>").
func secretKVDataPrefix(namespace, path string) string {
	return secretKVPrefix + "/data/" + secretKVKey(namespace, path) + "/"
}

// errSecretBackupUndecryptable is returned when the data of a secret in a
// backup can not be decrypted with the region's secret store core.
var errSecretBackupUndecryptable = errors.New("the secret's data in the backup can not be decrypted with this " +
	"region's secret store: the backup was made by another store, or by a store wrapped by another seal. " +
	"Bring that store back first with a full restore of the backup: the age copy of the backup carries its " +
	"secret store key, which the restore re-wraps with this region's seal; for backups without one, use the " +
	"recovery unseal with the old unseal shares ('norca operator unseal -recover'). Then restore the secret again")

func (r *backupRestorer) restoreSecret(item *structs.BackupRestoreItem, result *structs.BackupRestoreItemResult) error {
	srv := r.b.srv
	ns, path := item.Namespace, item.ID

	rec := &structs.SecretAccessEvent{
		Region:    srv.Region(),
		Server:    srv.config.NodeName,
		Namespace: ns,
		Path:      path,
		Operation: structs.SecretOpRestore,
		Accessor:  srv.secretAccessor(r.args.GetIdentity()),
		BackupID:  r.loaded.Record.ID,
	}
	if item.Version != nil {
		rec.BackupVersion = *item.Version
	}
	finish := func(err error) error {
		if r.args.DryRun {
			return err
		}
		rec.Result = secretAccessResult(err)
		if err != nil {
			rec.Error = err.Error()
		}
		if auditErr := srv.secretAudit(rec, true); err == nil {
			return auditErr
		}
		return err
	}

	aclObj, err := srv.ResolveACL(r.args)
	if err != nil {
		return err
	}
	if !aclObj.AllowSecretOperation(ns, path, acl.NamespaceCapabilityWriteSecret) {
		return finish(structs.ErrPermissionDenied)
	}

	backupRow, err := r.loaded.State.SecretByPath(nil, ns, path)
	if err != nil {
		return finish(err)
	}
	if backupRow == nil {
		return finish(fmt.Errorf("secret %q not found in the backup", path))
	}
	version := backupRow.CurrentVersion
	if item.Version != nil {
		version = *item.Version
	}
	rec.BackupVersion = version
	result.BackupVersion = version
	switch sv := backupRow.Version(version); {
	case sv == nil:
		return finish(fmt.Errorf("version %d of secret %q is not in the backup (it has versions %s)",
			version, path, secretVersionList(backupRow)))
	case sv.Destroyed:
		return finish(fmt.Errorf("version %d of secret %q is destroyed in the backup", version, path))
	}

	if err := (&Secret{srv: srv}).validateWrite(ns, path); err != nil {
		return finish(err)
	}
	current, err := srv.State().SecretByPath(nil, ns, path)
	if err != nil {
		return finish(err)
	}

	// The metadata: the backup's replaces the current one with
	// WithMetadata, and is used for a secret that does not exist.
	var meta, metaIfNew *structs.SecretMeta
	backupMeta := backupRow.SecretMeta.Copy()
	if item.WithMetadata {
		meta = backupMeta
	} else {
		metaIfNew = backupMeta
	}
	if item.WithMetadata || current == nil {
		if err := backupMeta.Validate(); err != nil {
			return finish(fmt.Errorf("the metadata of secret %q in the backup is invalid: %w", path, err))
		}
	}

	from := fmt.Sprintf("the value of version %d of secret %q in backup %s", version, path, r.loaded.Record.ID)
	if current == nil {
		result.Action = "create"
		result.NewVersion = 1
		result.Description = fmt.Sprintf("create the secret with %s as version 1, with the backup's metadata", from)
	} else {
		result.Action = "update"
		result.CurrentIndex = current.ModifyIndex
		result.NewVersion = current.CurrentVersion + 1
		result.Description = fmt.Sprintf("write %s as version %d (current version %d, modify index %d)",
			from, result.NewVersion, current.CurrentVersion, current.ModifyIndex)
		switch {
		case current.Destroyed:
			result.Description += "; the current version is destroyed"
		case current.Deleted:
			result.Description += "; the secret is soft deleted and becomes live again"
		}
		if item.WithMetadata {
			result.Description += "; with the backup's metadata"
		} else {
			result.Description += "; the current metadata is kept"
		}
	}

	// Decrypt the value now, also for a dry run, so that a backup whose
	// data the store can not read is reported before anything is written.
	items, err := srv.secretBackupItems(r.loaded.State, ns, path, version)
	if err != nil {
		return finish(err)
	}
	if err := items.Validate(); err != nil {
		return finish(fmt.Errorf("version %d of secret %q in the backup is invalid: %w", version, path, err))
	}

	if r.args.DryRun {
		if item.CheckIndex != nil {
			if err := checkSecretIndex(current, item.CheckIndex); err != nil {
				result.Description += fmt.Sprintf("; the check index %d does not match", *item.CheckIndex)
			}
		}
		return nil
	}

	if err := srv.secretAudit(rec, false); err != nil {
		return err
	}
	secret, index, err := srv.secretWrite(&secretWrite{
		namespace:  ns,
		path:       path,
		items:      items,
		meta:       meta,
		metaIfNew:  metaIfNew,
		checkIndex: item.CheckIndex,
	})
	if err == nil {
		rec.Version = secret.CurrentVersion
		result.NewVersion = secret.CurrentVersion
		result.Index = index
	} else {
		result.NewVersion = 0
	}
	return finish(err)
}

// secretVersionList lists the version numbers of a secret's row.
func secretVersionList(row *structs.Secret) string {
	if len(row.Versions) == 0 {
		return "none"
	}
	out := ""
	for i, v := range row.Versions {
		if i > 0 {
			out += ", "
		}
		out += strconv.FormatUint(v.Version, 10)
	}
	return out
}

// secretBackupItems decrypts the items of a version of a secret in a backup
// with the leader's core. The backup's blob of the version is only placed in
// the read context's batch: nothing is written.
func (s *Server) secretBackupItems(backupState *state.StateStore, namespace, path string, version uint64) (structs.SecretItems, error) {
	c, err := s.secretCore()
	if err != nil {
		return nil, errSecretUnavailable(err)
	}
	key := secretKVDataPrefix(namespace, path) + strconv.FormatUint(version, 10)
	blob, err := backupState.SecretStoreBlobByPath(nil, key)
	if err != nil {
		return nil, err
	}
	if blob == nil {
		return nil, fmt.Errorf("version %d of secret %q has no data in the backup", version, path)
	}

	ctx, batch := secretstore.WithBatch(context.Background())
	batch.Preload([]*structs.SecretStoreBlob{blob})
	e, err := c.Barrier().Get(ctx, key)
	switch {
	case errors.Is(err, barrier.ErrSealed), errors.Is(err, storage.ErrFenced):
		return nil, errSecretUnavailable(err)
	case err != nil:
		// The barrier's error names the storage key, never the value.
		return nil, fmt.Errorf("%w (%v)", errSecretBackupUndecryptable, err)
	case e == nil:
		return nil, fmt.Errorf("version %d of secret %q has no data in the backup", version, path)
	}
	var items structs.SecretItems
	if err := json.Unmarshal(e.Value, &items); err != nil {
		return nil, fmt.Errorf("version %d of secret %q in the backup is malformed", version, path)
	}
	return items, nil
}

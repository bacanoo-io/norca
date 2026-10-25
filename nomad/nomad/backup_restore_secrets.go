// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/secretstore"
	"github.com/hashicorp/nomad/secretstore/ubix/export/kv"
)

// Selective restore of a secret. A backup holds a secret as its metadata
// row and the barrier's ciphertext blobs of the KV engine (its version
// history and the data of each retained version). Restoring the secret
// replaces its current state with the one in the backup, with all retained
// versions: the backup's blobs are copied as they are, which works because
// they are encrypted with the barrier key of the store and bound to their
// storage path, both unchanged within a cluster. Before anything is
// written, every restored version is decrypted through the leader's core
// (the backup's blobs are preloaded into the write batch), so a backup whose
// barrier key differs from the running store's is refused instead of
// restoring unreadable data.

// secretKVDataPrefix is the barrier prefix of the version data of a secret,
// as the KV engine stores it ("<prefix>/data/<key>/<version>").
func secretKVDataPrefix(namespace, path string) string {
	return secretKVPrefix + "/data/" + secretKVKey(namespace, path) + "/"
}

// secretBlobs returns the barrier blobs of a secret in a state store: the KV
// metadata and the data of its versions, but not those of secrets below its
// path.
func secretBlobs(store *state.StateStore, namespace, path string) ([]*structs.SecretStoreBlob, error) {
	var out []*structs.SecretStoreBlob
	meta, err := store.SecretStoreBlobByPath(nil, secretKVMetaKey(namespace, path))
	if err != nil {
		return nil, err
	}
	if meta != nil {
		out = append(out, meta)
	}
	prefix := secretKVDataPrefix(namespace, path)
	iter, err := store.SecretStoreBlobsByPrefix(nil, prefix)
	if err != nil {
		return nil, err
	}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		blob := raw.(*structs.SecretStoreBlob)
		if _, err := strconv.ParseUint(strings.TrimPrefix(blob.Path, prefix), 10, 64); err != nil {
			// A blob of a secret below this path.
			continue
		}
		out = append(out, blob)
	}
	return out, nil
}

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
		return err
	}
	if backupRow == nil {
		return fmt.Errorf("secret %q not found in the backup", path)
	}
	blobs, err := secretBlobs(r.loaded.State, ns, path)
	if err != nil {
		return err
	}

	current, err := srv.State().SecretByPath(nil, ns, path)
	if err != nil {
		return err
	}
	versions := len(backupRow.Versions)
	if current == nil {
		result.Action = "create"
		result.Description = fmt.Sprintf("re-create secret %q with %d versions (current version %d)",
			path, versions, backupRow.CurrentVersion)
	} else {
		result.Action = "update"
		result.CurrentIndex = current.ModifyIndex
		result.Description = fmt.Sprintf("replace secret %q (modify index %d) with its %d versions in the backup (current version %d)",
			path, current.ModifyIndex, versions, backupRow.CurrentVersion)
		if item.CheckIndex == nil {
			msg := fmt.Sprintf("secret %q exists (modify index %d); give that check index to overwrite it",
				path, current.ModifyIndex)
			if r.args.DryRun {
				result.Description += "; " + msg
				return nil
			}
			return fmt.Errorf("%s", msg)
		}
	}
	if r.args.DryRun {
		return nil
	}

	checkIndex := uint64(0)
	if item.CheckIndex != nil {
		checkIndex = *item.CheckIndex
	}
	if err := srv.secretAudit(rec, false); err != nil {
		return err
	}
	secret, index, err := srv.secretRestore(backupRow, blobs, &checkIndex)
	if err == nil {
		rec.Version = secret.CurrentVersion
		result.Index = index
	}
	return finish(err)
}

// secretRestore replaces a secret with its state in a backup: its metadata
// row and its barrier blobs. It runs on the leader.
func (s *Server) secretRestore(backupRow *structs.Secret, blobs []*structs.SecretStoreBlob, checkIndex *uint64) (*structs.Secret, uint64, error) {
	c, unlock, err := s.lockedSecretCore()
	if err != nil {
		return nil, 0, err
	}
	defer unlock()

	ns, path := backupRow.Namespace, backupRow.Path
	store := s.fsm.State()
	existing, err := store.SecretByPath(nil, ns, path)
	if err != nil {
		return nil, 0, err
	}
	if err := checkSecretIndex(existing, checkIndex); err != nil {
		return nil, 0, err
	}

	ctx, batch := secretstore.WithBatch(context.Background())
	b := c.Barrier()

	// Remove the current blobs of the secret, then add the backup's.
	currentBlobs, err := secretBlobs(store, ns, path)
	if err != nil {
		return nil, 0, err
	}
	for _, blob := range currentBlobs {
		if err := b.Delete(ctx, blob.Path); err != nil {
			return nil, 0, err
		}
	}
	batch.Preload(blobs)

	hashKey, created, err := secretValueHashKey(ctx, b)
	if err != nil {
		return nil, 0, err
	}
	eng := kv.New(b, secretKVPrefix)
	kvMeta, err := eng.ReadMetadata(ctx, secretKVKey(ns, path))
	if err != nil {
		return nil, 0, fmt.Errorf("the secret's version history in the backup can not be read with this store's barrier key "+
			"(restoring it requires the seal the backup was made with): %w", err)
	}

	row := secretRow(nil, ns, path, *backupRow.SecretMeta.Copy(), kvMeta, time.Now())
	row.CreateTime = backupRow.CreateTime
	for _, sv := range row.Versions {
		if sv.Destroyed {
			continue
		}
		e, err := b.Get(ctx, secretKVDataPrefix(ns, path)+strconv.FormatUint(sv.Version, 10))
		if err != nil {
			return nil, 0, fmt.Errorf("version %d in the backup can not be decrypted with this store's barrier key: %w", sv.Version, err)
		}
		if e == nil {
			return nil, 0, fmt.Errorf("version %d is missing from the backup", sv.Version)
		}
		var items structs.SecretItems
		if err := json.Unmarshal(e.Value, &items); err != nil {
			return nil, 0, fmt.Errorf("version %d in the backup is malformed: %w", sv.Version, err)
		}
		sv.ValueHash = secretValueHash(hashKey, ns, path, items)
		if sv.Version == row.CurrentVersion {
			row.ItemKeys = items.Keys()
		}
	}
	secretRowCurrent(row)

	req := &structs.SecretApplyRequest{
		Secret:      row,
		Blobs:       batch.Blobs(),
		DeleteBlobs: batch.Deletes(),
	}
	if existing != nil {
		req.ExpectedIndex = existing.ModifyIndex
	}
	if created {
		req.CreateBlobs = []string{secretHMACKeyPath}
	}
	return s.secretApply(req)
}

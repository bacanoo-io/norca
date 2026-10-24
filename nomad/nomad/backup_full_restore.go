// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/helper/snapshot"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/backup"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// maxStoredPreviewObjects bounds the changed objects kept in the restore
// record of a full restore; the response of the request has them all.
const maxStoredPreviewObjects = 250

// Changes of an object in the preview of a full restore.
const (
	fullRestoreRemoved  = "removed"
	fullRestoreRestored = "restored"
	fullRestoreReverted = "reverted"
)

// fullRestoreIdentity checks that the caller may request, approve or reject
// a full restore, and returns its identity and token name. A full restore
// needs a management token, and ACLs must be enabled: the two people of the
// two-person approval are told apart by their tokens.
func (b *Backup) fullRestoreIdentity(args structs.RequestWithIdentity) (string, string, error) {
	if !b.srv.config.ACLEnabled {
		return "", "", structs.NewErrRPCCodedf(http.StatusBadRequest,
			"a full restore needs ACLs enabled: two different management tokens request and approve it")
	}
	aclObj, err := b.srv.ResolveACL(args)
	if err != nil {
		return "", "", err
	}
	if aclObj == nil || !aclObj.IsManagement() {
		return "", "", structs.ErrPermissionDenied
	}
	identity := args.GetIdentity()
	token := identity.GetACLToken()
	if token == nil || token == structs.LeaderACLToken || token.AccessorID == "" {
		return "", "", structs.ErrPermissionDenied
	}
	return identity.String(), token.Name, nil
}

// checkFullRestore returns an error if the backup can't replace the state
// of this region.
func (b *Backup) checkFullRestore(record *structs.BackupRecord) error {
	if err := backup.CheckRestorable(record, b.srv.Region(), false); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}
	return nil
}

// requestFullRestore previews a full restore, or with a confirmation creates
// the pending request that a second administrator approves.
func (b *Backup) requestFullRestore(args *structs.BackupRestoreRequest, reply *structs.BackupRestoreResponse) error {
	identity, tokenName, err := b.fullRestoreIdentity(args)
	if err != nil {
		return err
	}
	if len(args.Items) > 0 {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "a full restore takes no items")
	}

	ctx, cancel := b.backupContext()
	defer cancel()
	loaded, err := b.load(ctx, args.ID, args.Destination, args.AgeIdentity)
	if err != nil {
		return err
	}
	if err := b.checkFullRestore(loaded.Record); err != nil {
		return err
	}
	preview, err := b.fullRestorePreview(loaded)
	if err != nil {
		return err
	}
	if args.DryRun {
		reply.Preview = preview
		return nil
	}
	if args.Confirm != b.srv.Region() {
		return structs.NewErrRPCCodedf(http.StatusBadRequest,
			"confirm a full restore with the name of the region (%q)", b.srv.Region())
	}

	if !b.srv.backup.TryLockRestore() {
		return structs.NewErrRPCCodedf(http.StatusConflict, "a full restore is in progress")
	}
	defer b.srv.backup.UnlockRestore()

	// Only one full restore request can wait for approval.
	now := time.Now().UTC()
	restores, err := b.srv.State().BackupRestoresList(nil)
	if err != nil {
		return err
	}
	for _, r := range restores {
		if r.Mode != structs.BackupRestoreModeFull || r.Status != structs.BackupRestoreStatusPending {
			continue
		}
		if r.Expired(now) {
			if _, err := b.expireRestore(r); err != nil {
				return err
			}
			continue
		}
		return structs.NewErrRPCCodedf(http.StatusConflict,
			"full restore %s is waiting for approval; reject it first", r.ID)
	}

	stored := *preview
	if len(stored.Objects) > maxStoredPreviewObjects {
		stored.Objects = stored.Objects[:maxStoredPreviewObjects]
		stored.Truncated = true
	}
	record := &structs.BackupRestore{
		ID:              uuid.Generate(),
		BackupID:        loaded.Record.ID,
		Mode:            structs.BackupRestoreModeFull,
		Status:          structs.BackupRestoreStatusPending,
		RequestedBy:     identity,
		RequestedByName: tokenName,
		Destination:     loaded.Destination,
		ExpiresAt:       now.Add(b.srv.backup.Config().ApprovalTTL),
		Preview:         &stored,
		CreateTime:      now,
		ModifyTime:      now,
	}
	index, err := b.writeRestoreRecord(record)
	if err != nil {
		return err
	}
	record.CreateIndex, record.ModifyIndex = index, index
	b.logger.Info("full restore requested", "audit", true, "restore_id", record.ID, "backup_id", record.BackupID,
		"requested_by", identity, "expires_at", record.ExpiresAt)

	reply.Restore = record
	reply.Preview = preview
	reply.Index = index
	return nil
}

// fullRestorePreview compares a backup with the current state.
func (b *Backup) fullRestorePreview(loaded *backup.Loaded) (*structs.BackupRestorePreview, error) {
	current := b.srv.fsm.State()
	preview := &structs.BackupRestorePreview{
		BackupID:    loaded.Record.ID,
		Region:      b.srv.Region(),
		BackupIndex: loaded.Record.RaftIndex,
		Counts:      map[string]int{},
	}
	preview.CurrentIndex, _ = current.LatestIndex()

	diffs, err := backupDiff(loaded.State, current, "", &backupContentsFilter{namespace: "*"})
	if err != nil {
		return nil, err
	}
	for _, d := range diffs {
		change := fullRestoreReverted
		switch d.Change {
		case structs.BackupDiffAdded:
			change = fullRestoreRemoved
		case structs.BackupDiffDeleted:
			change = fullRestoreRestored
		}
		preview.Counts[d.Type+"/"+change]++
	}
	preview.Objects = diffs

	ws := memdb.NewWatchSet()
	allocs, err := current.Allocs(ws, state.SortDefault)
	if err != nil {
		return nil, err
	}
	for raw := allocs.Next(); raw != nil; raw = allocs.Next() {
		alloc := raw.(*structs.Allocation)
		if alloc.TerminalStatus() {
			continue
		}
		if old, err := loaded.State.AllocByID(nil, alloc.ID); err != nil {
			return nil, err
		} else if old == nil {
			preview.AllocationsStopped++
		}
	}

	tokens, err := current.ACLTokens(ws, state.SortDefault)
	if err != nil {
		return nil, err
	}
	for raw := tokens.Next(); raw != nil; raw = tokens.Next() {
		token := raw.(*structs.ACLToken)
		if old, err := loaded.State.ACLTokenByAccessorID(nil, token.AccessorID); err != nil {
			return nil, err
		} else if old == nil {
			preview.ACLTokensRemoved++
		}
	}

	for _, key := range loaded.Keys {
		if _, err := b.srv.encrypter.GetKey(key.Meta.KeyID); err != nil {
			preview.KeysAdded = append(preview.KeysAdded, key.Meta.KeyID)
		}
	}

	if b.srv.config.AuthoritativeRegion == "" || b.srv.config.AuthoritativeRegion == b.srv.Region() {
		preview.Warnings = append(preview.Warnings,
			"this is the authoritative region: the global ACL policies and tokens roll back in every region")
	}
	if preview.AllocationsStopped > 0 {
		preview.Warnings = append(preview.Warnings, fmt.Sprintf(
			"%d running allocations are not in the backup; their clients stop them", preview.AllocationsStopped))
	}
	if preview.ACLTokensRemoved > 0 {
		preview.Warnings = append(preview.Warnings, fmt.Sprintf(
			"%d ACL tokens were created after the backup and are deleted", preview.ACLTokensRemoved))
	}
	if loaded.Keys == nil {
		preview.Warnings = append(preview.Warnings,
			"the backup has no keyring: Variables encrypted with keys missing from the current keyring are unreadable")
	}
	preview.Warnings = append(preview.Warnings,
		"every change since the backup is lost; an automatic backup of the current state is taken first")
	return preview, nil
}

// expireRestore marks a pending restore past its approval period expired.
func (b *Backup) expireRestore(record *structs.BackupRestore) (uint64, error) {
	expired := record.Copy()
	expired.Status = structs.BackupRestoreStatusExpired
	expired.StatusDescription = "the request was not approved in time"
	expired.ModifyTime = time.Now().UTC()
	index, err := b.writeRestoreRecord(expired)
	if err == nil {
		b.logger.Info("full restore request expired", "audit", true, "restore_id", record.ID)
	}
	return index, err
}

// pendingFullRestore returns a pending full restore request, expiring it if
// its approval period is over.
func (b *Backup) pendingFullRestore(id string) (*structs.BackupRestore, error) {
	if id == "" {
		return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "missing restore ID")
	}
	record, err := b.srv.State().BackupRestoreByID(nil, id)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, structs.NewErrRPCCodedf(http.StatusNotFound, "restore %q not found", id)
	}
	if record.Mode != structs.BackupRestoreModeFull {
		return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "restore %s is not a full restore", id)
	}
	if record.Expired(time.Now()) {
		if _, err := b.expireRestore(record); err != nil {
			return nil, err
		}
		return nil, structs.NewErrRPCCodedf(http.StatusBadRequest,
			"full restore %s expired at %s without approval", id, record.ExpiresAt.Format(time.RFC3339))
	}
	if record.Status != structs.BackupRestoreStatusPending {
		return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "full restore %s is %s", id, record.Status)
	}
	return record, nil
}

// ApproveRestore approves a pending full restore as the second
// administrator and runs it: an automatic backup of the current state, the
// keys of the backup into the keystore of every server, then the Raft state
// of the backup with the path of "operator snapshot restore".
func (b *Backup) ApproveRestore(args *structs.BackupRestoreApproveRequest, reply *structs.BackupRestoreResponse) error {
	authErr := b.srv.Authenticate(b.ctx, args)
	if done, err := b.srv.forward("Backup.ApproveRestore", args, args, reply); done {
		return err
	}
	b.srv.MeasureRPCRate("backup", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "backup", "approve_restore"}, time.Now())

	identity, _, err := b.fullRestoreIdentity(args)
	if err != nil {
		return err
	}
	if err := b.checkVersion(); err != nil {
		return err
	}
	if !b.srv.backup.TryLockRestore() {
		return structs.NewErrRPCCodedf(http.StatusConflict, "a full restore is in progress")
	}
	defer b.srv.backup.UnlockRestore()

	record, err := b.pendingFullRestore(args.RestoreID)
	if err != nil {
		return err
	}
	if record.RequestedBy == identity {
		return structs.NewErrRPCCodedf(http.StatusForbidden,
			"the requester of a full restore can't approve it; a second administrator must")
	}

	ctx, cancel := b.backupContext()
	defer cancel()
	backupRecord, err := b.lookupRecord(ctx, record.BackupID, record.Destination)
	if err != nil {
		return err
	}
	if err := b.checkFullRestore(backupRecord); err != nil {
		return err
	}
	ids, err := backup.ParseIdentities(args.AgeIdentity)
	if err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}
	dest, st, err := b.srv.backup.ReadDestination(backupRecord, record.Destination)
	if err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}
	// The identity must decrypt the backup before the approval is recorded.
	if err := backup.CheckIdentity(ctx, dest, backupRecord, st, ids); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}

	record = record.Copy()
	record.Status = structs.BackupRestoreStatusApproved
	record.Approvals = append(record.Approvals, identity)
	record.ApprovedAt = time.Now().UTC()
	record.ModifyTime = record.ApprovedAt
	if _, err := b.writeRestoreRecord(record); err != nil {
		return err
	}
	b.logger.Info("full restore approved", "audit", true, "restore_id", record.ID, "backup_id", record.BackupID,
		"requested_by", record.RequestedBy, "approved_by", identity)

	restored, err := b.runFullRestore(ctx, record, backupRecord, dest, st, ids)
	reply.Restore = restored
	if restored != nil {
		reply.Index = restored.ModifyIndex
	}
	return err
}

// RejectRestore rejects a pending full restore. The requester can withdraw
// their own request.
func (b *Backup) RejectRestore(args *structs.BackupRestoreRejectRequest, reply *structs.BackupRestoreResponse) error {
	authErr := b.srv.Authenticate(b.ctx, args)
	if done, err := b.srv.forward("Backup.RejectRestore", args, args, reply); done {
		return err
	}
	b.srv.MeasureRPCRate("backup", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "backup", "reject_restore"}, time.Now())

	identity, _, err := b.fullRestoreIdentity(args)
	if err != nil {
		return err
	}
	if !b.srv.backup.TryLockRestore() {
		return structs.NewErrRPCCodedf(http.StatusConflict, "a full restore is in progress")
	}
	defer b.srv.backup.UnlockRestore()

	record, err := b.pendingFullRestore(args.RestoreID)
	if err != nil {
		return err
	}
	record = record.Copy()
	record.Status = structs.BackupRestoreStatusRejected
	record.RejectedBy = identity
	record.StatusDescription = "rejected"
	if args.Reason != "" {
		record.StatusDescription = "rejected: " + args.Reason
	}
	record.ModifyTime = time.Now().UTC()
	index, err := b.writeRestoreRecord(record)
	if err != nil {
		return err
	}
	b.logger.Info("full restore rejected", "audit", true, "restore_id", record.ID, "rejected_by", identity,
		"reason", args.Reason)
	record.ModifyIndex = index
	reply.Restore = record
	reply.Index = index
	return nil
}

// runFullRestore runs an approved full restore and returns the final
// restore record.
func (b *Backup) runFullRestore(ctx context.Context, record *structs.BackupRestore, backupRecord *structs.BackupRecord,
	dest backup.Destination, st *structs.BackupDestinationStatus, ids backup.Identities) (*structs.BackupRestore, error) {

	write := func() {
		record.ModifyTime = time.Now().UTC()
		index, err := b.writeRestoreRecord(record)
		if err != nil {
			b.logger.Error("failed to record the state of a full restore", "restore_id", record.ID, "error", err)
			return
		}
		record.ModifyIndex = index
	}
	fail := func(err error) (*structs.BackupRestore, error) {
		record.Status = structs.BackupRestoreStatusFailed
		record.StatusDescription = err.Error()
		write()
		b.logger.Error("full restore failed", "audit", true, "restore_id", record.ID, "error", err)
		return record, err
	}

	record.Status = structs.BackupRestoreStatusRunning
	write()
	b.logger.Info("full restore started", "audit", true, "restore_id", record.ID, "backup_id", backupRecord.ID)

	// 1. The automatic, pinned backup of the current state, to undo the
	// restore. Nothing is restored without it.
	pre, err := b.srv.backup.Save(backup.SaveOptions{
		Trigger: structs.BackupTriggerPreRestore,
		Pin:     true,
		Labels: map[string]string{
			"restore": record.ID,
			"note":    "automatic backup before the full restore of " + backupRecord.ID,
		},
	})
	if err != nil {
		return fail(fmt.Errorf("the automatic pre-restore backup failed, nothing was restored: %w", err))
	}
	record.PreRestoreBackupID = pre.ID
	write()

	// 2. The snapshot archive, verified against its checksum before
	// anything changes.
	archive, err := os.CreateTemp("", "norca-restore-*.snap")
	if err != nil {
		return fail(err)
	}
	defer func() {
		archive.Close()
		os.Remove(archive.Name())
	}()
	if _, err := backup.DownloadState(ctx, dest, backupRecord, st, ids, archive); err != nil {
		return fail(err)
	}
	keys, err := backup.ReadKeys(ctx, dest, backupRecord, st, ids)
	if err != nil {
		return fail(err)
	}

	// 3. The keys of the backup into the keystore of every server, so the
	// restored Variables and workload identities stay readable.
	var notes []string
	if keys != nil {
		resp, err := b.srv.restoreKeyring(keys, b.srv.getLeaderAcl())
		if err != nil {
			return fail(fmt.Errorf("failed to restore the keyring, nothing was restored: %w", err))
		}
		for name, e := range resp.Errors {
			notes = append(notes, fmt.Sprintf("keys not restored on %s (keyring replication retries): %s", name, e))
		}
	} else {
		notes = append(notes, "the backup has no keyring")
	}

	// The backup and restore records are those of the stores, not of the
	// point in time of the backup: keep the current ones.
	store := b.srv.State()
	backups, err := store.BackupsList(nil)
	if err != nil {
		return fail(err)
	}
	restores, err := store.BackupRestoresList(nil)
	if err != nil {
		return fail(err)
	}

	// 4. The Raft state.
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	if err := b.srv.restoreRaftSnapshot(archive); err != nil {
		return fail(fmt.Errorf("failed to restore the state: %w", err))
	}

	if err := b.reapplyBackupRecords(backups, restores, record.ID); err != nil {
		notes = append(notes, "failed to keep the current backup records: "+err.Error())
	}

	record.Status = structs.BackupRestoreStatusComplete
	record.StatusDescription = fmt.Sprintf("restored backup %s (Raft index %d); undo with backup %s",
		backupRecord.ID, backupRecord.RaftIndex, pre.ID)
	if len(notes) > 0 {
		record.StatusDescription += "; " + strings.Join(notes, "; ")
	}
	write()
	b.logger.Info("full restore finished", "audit", true, "restore_id", record.ID, "backup_id", backupRecord.ID,
		"pre_restore_backup_id", pre.ID)
	return record, nil
}

// reapplyBackupRecords writes the backup and restore records of before a
// full restore over those of the restored state: backups deleted since the
// backup was taken are removed, newer ones (the pre-restore backup among
// them) and the restore records are kept.
func (b *Backup) reapplyBackupRecords(backups []*structs.BackupRecord, restores []*structs.BackupRestore, skipID string) error {
	store := b.srv.State()
	restored, err := store.BackupsList(nil)
	if err != nil {
		return err
	}
	var deleted []string
	for _, r := range restored {
		if !slices.ContainsFunc(backups, func(c *structs.BackupRecord) bool { return c.ID == r.ID }) {
			deleted = append(deleted, r.ID)
		}
	}
	if len(deleted) > 0 {
		if _, _, err := b.srv.raftApply(structs.BackupDeleteRequestType|structs.IgnoreUnknownTypeFlag,
			&structs.BackupDeleteRequest{IDs: deleted}); err != nil {
			return err
		}
	}

	var upsert []*structs.BackupRecord
	for _, c := range backups {
		r, err := store.BackupByID(nil, c.ID)
		if err != nil {
			return err
		}
		if r == nil || r.ModifyIndex != c.ModifyIndex {
			upsert = append(upsert, c)
		}
	}
	if len(upsert) > 0 {
		if _, _, err := b.srv.raftApply(structs.BackupUpsertRequestType|structs.IgnoreUnknownTypeFlag,
			&structs.BackupUpsertRequest{Backups: upsert}); err != nil {
			return err
		}
	}

	var restoreUpsert []*structs.BackupRestore
	for _, c := range restores {
		if c.ID == skipID {
			continue
		}
		r, err := store.BackupRestoreByID(nil, c.ID)
		if err != nil {
			return err
		}
		if r == nil || r.ModifyIndex != c.ModifyIndex {
			restoreUpsert = append(restoreUpsert, c)
		}
	}
	if len(restoreUpsert) > 0 {
		if _, _, err := b.srv.raftApply(structs.BackupRestoreUpsertRequestType|structs.IgnoreUnknownTypeFlag,
			&structs.BackupRestoreUpsertRequest{Restores: restoreUpsert}); err != nil {
			return err
		}
	}
	return nil
}

// restoreRaftSnapshot restores a Raft snapshot archive on the leader and
// re-runs the leader actions on the new state, like snapshotRestore of the
// Operator endpoint: the leader's subsystems (eval broker, plan queue,
// deployment watcher, ...) are stopped and started again on the restored
// state.
func (s *Server) restoreRaftSnapshot(archive io.Reader) error {
	if err := snapshot.Restore(s.logger.Named("snapshot"), archive, s.raft); err != nil {
		return err
	}

	timeout := time.After(time.Minute)
	errCh := make(chan error, 1)
	select {
	case s.reassertLeaderCh <- errCh:
	case <-timeout:
		return errors.New("timed out waiting to re-run leader actions")
	case <-s.shutdownCh:
		return errors.New("server is shutting down")
	}
	select {
	case err := <-errCh:
		return err
	case <-timeout:
		return errors.New("timed out waiting for re-run of leader actions")
	case <-s.shutdownCh:
		return errors.New("server is shutting down")
	}
}

// GetRestore returns a restore record, with the preview of a full restore.
func (b *Backup) GetRestore(args *structs.BackupRestoreSpecificRequest, reply *structs.SingleBackupRestoreResponse) error {
	authErr := b.srv.Authenticate(b.ctx, args)
	if done, err := b.srv.forward("Backup.GetRestore", args, args, reply); done {
		return err
	}
	b.srv.MeasureRPCRate("backup", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "backup", "get_restore"}, time.Now())

	aclObj, err := b.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowBackupRead() {
		return structs.ErrPermissionDenied
	}

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			record, err := store.BackupRestoreByID(ws, args.RestoreID)
			if err != nil {
				return err
			}
			reply.Restore = record
			if record != nil {
				reply.Index = record.ModifyIndex
			} else {
				index, err := store.Index(state.TableBackupRestores)
				if err != nil {
					return err
				}
				reply.Index = max(1, index)
			}
			b.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return b.srv.blockingRPC(&opts)
}

// ListRestores returns the restore records of the region, newest first,
// without the changed objects of the previews.
func (b *Backup) ListRestores(args *structs.BackupRestoreListRequest, reply *structs.BackupRestoreListResponse) error {
	authErr := b.srv.Authenticate(b.ctx, args)
	if done, err := b.srv.forward("Backup.ListRestores", args, args, reply); done {
		return err
	}
	b.srv.MeasureRPCRate("backup", structs.RateMetricList, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "backup", "list_restores"}, time.Now())

	aclObj, err := b.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowBackupRead() {
		return structs.ErrPermissionDenied
	}

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, store *state.StateStore) error {
			records, err := store.BackupRestoresList(ws)
			if err != nil {
				return err
			}
			reply.Restores = make([]*structs.BackupRestore, 0, len(records))
			for _, r := range records {
				if r.Preview != nil {
					r = r.Copy()
					preview := *r.Preview
					preview.Objects = nil
					r.Preview = &preview
				}
				reply.Restores = append(reply.Restores, r)
			}
			index, err := store.Index(state.TableBackupRestores)
			if err != nil {
				return err
			}
			reply.Index = max(1, index)
			b.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return b.srv.blockingRPC(&opts)
}

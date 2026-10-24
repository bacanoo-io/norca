// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/backup"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// backupDownloadURLExpiry is the validity of a pre-signed download URL.
const backupDownloadURLExpiry = 15 * time.Minute

// Backup endpoint lists, takes, browses, compares and restores the backups
// of a region. Writes and restores run on the leader of the region.
type Backup struct {
	srv    *Server
	ctx    *RPCContext
	logger hclog.Logger
}

func NewBackupEndpoint(srv *Server, ctx *RPCContext) *Backup {
	return &Backup{srv: srv, ctx: ctx, logger: srv.logger.Named("backup")}
}

// List returns the backups of the region, newest first, and the state of
// the schedule.
func (b *Backup) List(args *structs.BackupListRequest, reply *structs.BackupListResponse) error {
	authErr := b.srv.Authenticate(b.ctx, args)
	if done, err := b.srv.forward("Backup.List", args, args, reply); done {
		return err
	}
	b.srv.MeasureRPCRate("backup", structs.RateMetricList, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "backup", "list"}, time.Now())

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
			records, err := store.BackupsList(ws)
			if err != nil {
				return err
			}
			reply.Backups = records
			reply.Schedule = b.srv.backup.Config().ScheduleStatus(records, b.srv.Region(), time.Now())

			index, err := store.Index(state.TableBackups)
			if err != nil {
				return err
			}
			reply.Index = max(1, index)
			b.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return b.srv.blockingRPC(&opts)
}

// Get returns a backup record.
func (b *Backup) Get(args *structs.BackupSpecificRequest, reply *structs.SingleBackupResponse) error {
	authErr := b.srv.Authenticate(b.ctx, args)
	if done, err := b.srv.forward("Backup.Get", args, args, reply); done {
		return err
	}
	b.srv.MeasureRPCRate("backup", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "backup", "get"}, time.Now())

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
			record, err := store.BackupByID(ws, args.ID)
			if err != nil {
				return err
			}
			reply.Backup = record
			if record != nil {
				reply.Index = record.ModifyIndex
			} else {
				index, err := store.Index(state.TableBackups)
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

// checkBackupsEnabled returns an error unless every server supports backups.
func (b *Backup) checkVersion() error {
	if !ServersMeetMinimumNorcaVersion(b.srv.serf.Members(), b.srv.Region(), false) {
		return fmt.Errorf("all servers must be running Norca %v or later to use backups", minNorcaFeaturesVersion)
	}
	return nil
}

// Save takes a manual backup on the leader.
func (b *Backup) Save(args *structs.BackupSaveRequest, reply *structs.BackupSaveResponse) error {
	authErr := b.srv.Authenticate(b.ctx, args)
	if done, err := b.srv.forward("Backup.Save", args, args, reply); done {
		return err
	}
	b.srv.MeasureRPCRate("backup", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "backup", "save"}, time.Now())

	aclObj, err := b.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowBackupWrite() {
		return structs.ErrPermissionDenied
	}
	if err := b.checkVersion(); err != nil {
		return err
	}
	if err := structs.ValidateBackupLabels(args.Labels); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid labels: %v", err)
	}

	opts := backup.SaveOptions{Trigger: structs.BackupTriggerManual, Labels: args.Labels, Pin: args.Pin}
	if args.Detach {
		started := make(chan string, 1)
		errCh := make(chan error, 1)
		opts.Started = func(id string) { started <- id }
		go func() {
			if _, err := b.srv.backup.Save(opts); err != nil {
				b.logger.Error("manual backup failed", "error", err)
				errCh <- err
			}
		}()
		select {
		case id := <-started:
			reply.ID = id
			return nil
		case err := <-errCh:
			return err
		}
	}

	record, err := b.srv.backup.Save(opts)
	if record != nil {
		reply.ID = record.ID
		reply.Backup = record
		reply.Index = record.ModifyIndex
	}
	return err
}

// Update pins, unpins or labels a backup.
func (b *Backup) Update(args *structs.BackupUpdateRequest, reply *structs.BackupUpdateResponse) error {
	authErr := b.srv.Authenticate(b.ctx, args)
	if done, err := b.srv.forward("Backup.Update", args, args, reply); done {
		return err
	}
	b.srv.MeasureRPCRate("backup", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "backup", "update"}, time.Now())

	aclObj, err := b.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowBackupWrite() {
		return structs.ErrPermissionDenied
	}
	if err := b.checkVersion(); err != nil {
		return err
	}

	existing, err := b.srv.State().BackupByID(nil, args.ID)
	if err != nil {
		return err
	}
	if existing == nil {
		return structs.NewErrRPCCodedf(http.StatusNotFound, "backup %q not found", args.ID)
	}

	record := existing.Copy()
	if args.Pinned != nil {
		record.Pinned = *args.Pinned
	}
	if args.Labels != nil {
		record.Labels = make(map[string]string, len(args.Labels))
		for k, v := range args.Labels {
			record.Labels[k] = v
		}
	}
	for _, k := range args.RemoveLabels {
		delete(record.Labels, k)
	}
	if err := structs.ValidateBackupLabels(record.Labels); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid labels: %v", err)
	}

	index, err := b.srv.backup.Update(record)
	if err != nil {
		return err
	}
	record.ModifyIndex = index
	reply.Backup = record
	reply.Index = index
	return nil
}

// Delete deletes backups, honoring pinning and min_keep.
func (b *Backup) Delete(args *structs.BackupDeleteRequest, reply *structs.GenericResponse) error {
	authErr := b.srv.Authenticate(b.ctx, args)
	if done, err := b.srv.forward("Backup.Delete", args, args, reply); done {
		return err
	}
	b.srv.MeasureRPCRate("backup", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "backup", "delete"}, time.Now())

	aclObj, err := b.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowBackupWrite() {
		return structs.ErrPermissionDenied
	}
	if err := b.checkVersion(); err != nil {
		return err
	}
	if len(args.IDs) == 0 {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "must specify at least one backup to delete")
	}

	records, err := b.srv.State().BackupsList(nil)
	if err != nil {
		return err
	}
	retention := b.srv.backup.Config().RetentionOrDefault()
	for _, id := range args.IDs {
		idx := slices.IndexFunc(records, func(r *structs.BackupRecord) bool { return r.ID == id })
		if idx < 0 {
			return structs.NewErrRPCCodedf(http.StatusNotFound, "backup %q not found", id)
		}
		record := records[idx]
		if err := backup.CanDelete(record, records, retention); err != nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
		}
		index, err := b.srv.backup.Delete(record)
		if err != nil {
			return err
		}
		reply.Index = index
		records = slices.Delete(records, idx, idx+1)
	}
	return nil
}

// backupContext returns the context of a request that reads backups.
func (b *Backup) backupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(b.srv.shutdownCtx, b.srv.backup.Config().Timeout)
}

// lookupRecord returns the record of a backup: from the backup_status
// table, or else from its manifest in a destination.
func (b *Backup) lookupRecord(ctx context.Context, id, destination string) (*structs.BackupRecord, error) {
	if id == "" {
		return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "missing backup ID")
	}
	record, err := b.srv.State().BackupByID(nil, id)
	if err != nil {
		return nil, err
	}
	if record != nil {
		if !record.Usable() {
			return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "backup %s has status %q", id, record.Status)
		}
		return record, nil
	}
	if _, _, err := backup.ParseID(id); err != nil {
		return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}
	record, err = b.srv.backup.LoadRecord(ctx, id, destination)
	if err != nil {
		return nil, structs.NewErrRPCCodedf(http.StatusNotFound, "%v", err)
	}
	return record, nil
}

// load loads a backup into a separate state store.
func (b *Backup) load(ctx context.Context, id, destination, identity string) (*backup.Loaded, error) {
	record, err := b.lookupRecord(ctx, id, destination)
	if err != nil {
		return nil, err
	}
	if identity == "" {
		return nil, structs.NewErrRPCCodedf(http.StatusBadRequest,
			"an age identity is required to decrypt backup %s", id)
	}
	loaded, err := b.srv.backup.Load(ctx, record, destination, identity,
		backupRestoreFunc(b.logger, b.srv.Region()))
	if err != nil {
		return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}
	return loaded, nil
}

// Contents browses one content type of a backup. Requests may be served by
// any server with AllowStale.
func (b *Backup) Contents(args *structs.BackupContentsRequest, reply *structs.BackupContentsResponse) error {
	authErr := b.srv.Authenticate(b.ctx, args)
	if done, err := b.srv.forward("Backup.Contents", args, args, reply); done {
		return err
	}
	b.srv.MeasureRPCRate("backup", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "backup", "contents"}, time.Now())

	aclObj, err := b.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowBackupRead() {
		return structs.ErrPermissionDenied
	}

	ctx, cancel := b.backupContext()
	defer cancel()
	loaded, err := b.load(ctx, args.ID, args.Destination, args.AgeIdentity)
	if err != nil {
		return err
	}

	contentType := args.Type
	if contentType == "" {
		contentType = structs.BackupContentJobs
	}
	contents, err := backupContents(loaded, contentType, &backupContentsFilter{
		aclObj:    aclObj,
		namespace: args.RequestNamespace(),
		jobID:     args.JobID,
		prefix:    args.PathPrefix,
	})
	if err != nil {
		return err
	}
	reply.Contents = contents
	reply.Index = max(1, loaded.Record.ModifyIndex)
	b.srv.setQueryMeta(&reply.QueryMeta)
	return nil
}

// Diff compares a backup with the current state or another backup.
func (b *Backup) Diff(args *structs.BackupDiffRequest, reply *structs.BackupDiffResponse) error {
	authErr := b.srv.Authenticate(b.ctx, args)
	if done, err := b.srv.forward("Backup.Diff", args, args, reply); done {
		return err
	}
	b.srv.MeasureRPCRate("backup", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "backup", "diff"}, time.Now())

	aclObj, err := b.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowBackupRead() {
		return structs.ErrPermissionDenied
	}

	ctx, cancel := b.backupContext()
	defer cancel()
	from, err := b.load(ctx, args.ID, args.Destination, args.AgeIdentity)
	if err != nil {
		return err
	}

	diff := &structs.BackupDiff{From: from.Record.ID, FromIndex: from.Record.RaftIndex}
	var to *state.StateStore
	switch args.Against {
	case "", "current":
		to = b.srv.fsm.State()
		diff.To = "current"
		diff.ToIndex, _ = to.LatestIndex()
	default:
		other, err := b.load(ctx, args.Against, args.Destination, args.AgeIdentity)
		if err != nil {
			return err
		}
		to = other.State
		diff.To = other.Record.ID
		diff.ToIndex = other.Record.RaftIndex
	}

	filter := &backupContentsFilter{
		aclObj:    aclObj,
		namespace: args.RequestNamespace(),
		jobID:     args.JobID,
	}
	diff.Objects, err = backupDiff(from.State, to, args.Type, filter)
	if err != nil {
		return err
	}

	// The job diff of a single job, from the backup to the other side.
	if args.JobID != "" && filter.job(args.RequestNamespace(), args.JobID) {
		fromJob, err := from.State.JobByID(nil, args.RequestNamespace(), args.JobID)
		if err != nil {
			return err
		}
		toJob, err := to.JobByID(nil, args.RequestNamespace(), args.JobID)
		if err != nil {
			return err
		}
		if fromJob != nil && toJob != nil {
			if diff.JobDiff, err = fromJob.Diff(toJob, true); err != nil {
				return err
			}
		}
	}

	reply.Diff = diff
	reply.Index = max(1, from.Record.ModifyIndex)
	b.srv.setQueryMeta(&reply.QueryMeta)
	return nil
}

// DownloadURL returns a short-lived pre-signed download URL of a backup
// object.
func (b *Backup) DownloadURL(args *structs.BackupDownloadURLRequest, reply *structs.BackupDownloadURLResponse) error {
	authErr := b.srv.Authenticate(b.ctx, args)
	if done, err := b.srv.forward("Backup.DownloadURL", args, args, reply); done {
		return err
	}
	b.srv.MeasureRPCRate("backup", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "backup", "download_url"}, time.Now())

	aclObj, err := b.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	// Downloading the encrypted objects needs write permission.
	if !aclObj.AllowBackupWrite() {
		return structs.ErrPermissionDenied
	}

	ctx, cancel := b.backupContext()
	defer cancel()
	record, err := b.lookupRecord(ctx, args.ID, args.Destination)
	if err != nil {
		return err
	}
	object := args.Object
	if object == "" {
		object = structs.BackupObjectState
	}
	if _, err := backup.ObjectKey(object); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}
	url, err := b.srv.backup.PresignURL(ctx, record, args.Destination, object, backupDownloadURLExpiry)
	if err != nil {
		if errors.Is(err, backup.ErrNotFound) {
			return structs.NewErrRPCCodedf(http.StatusNotFound, "%v", err)
		}
		return err
	}
	reply.URL = url
	reply.ExpiresAt = time.Now().Add(backupDownloadURLExpiry).UTC()
	b.srv.setQueryMeta(&reply.QueryMeta)
	return nil
}

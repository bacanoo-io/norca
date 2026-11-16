// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/hashicorp/cronexpr"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
)

// The operator of snapshot schedules takes a CSI snapshot of a volume at each
// time of its cron schedule and deletes the old ones its retention rule lets
// go. Norca keeps no record of snapshots in its state, so the snapshots of a
// schedule are listed in its status, and only those are ever deleted.
//
// The name of a snapshot is derived from the time it was scheduled for. CSI
// creates a snapshot of a name once and answers again with the one it has,
// so a leader that takes over a snapshot in progress, or tries a failed one
// again, never makes a second one. A time missed while there was no leader is
// taken once when the next leader comes, not once per missed time.
//
// Snapshots are crash-consistent: what the volume holds at the moment of the
// snapshot, without any help from the application.

var (
	// snapshotRetryBase and snapshotRetryMax bound the wait after a failed
	// snapshot: it doubles with every failure in a row. A scheduled time
	// that comes earlier is taken at its time.
	snapshotRetryBase = time.Minute
	snapshotRetryMax  = time.Hour

	// snapshotReadyPoll is how often a snapshot the plugin has not finished
	// is asked for again.
	snapshotReadyPoll = 30 * time.Second
)

// The conditions of a snapshot schedule besides Ready and Degraded.
const (
	snapshotConditionFailed      = "Failed"
	snapshotConditionUnsupported = "Unsupported"
	snapshotConditionSuspended   = "Suspended"
)

// snapshotScheduleSpec is the spec of a VolumeSnapshotSchedule object.
type snapshotScheduleSpec struct {
	Volume          string            `json:"volume"`
	Schedule        string            `json:"schedule"`
	TimeZone        string            `json:"time_zone"`
	Keep            snapshotKeep      `json:"keep"`
	NamePrefix      string            `json:"name_prefix"`
	Parameters      map[string]string `json:"parameters"`
	SecretPath      string            `json:"secret_path"`
	DeleteSnapshots bool              `json:"delete_snapshots"`
	Suspend         bool              `json:"suspend"`
}

// snapshotKeep is the retention rule: a snapshot is kept while one of the
// rules that are set keeps it.
type snapshotKeep struct {
	// Last keeps the newest snapshots.
	Last int `json:"last"`

	// MaxAge keeps the snapshots younger than it.
	MaxAge string `json:"max_age"`
}

// snapshotScheduleStatus is the status of a VolumeSnapshotSchedule object.
type snapshotScheduleStatus struct {
	Plugin    string               `json:"plugin,omitempty"`
	LastRun   string               `json:"lastRun,omitempty"`
	NextRun   string               `json:"nextRun,omitempty"`
	Snapshots []*scheduledSnapshot `json:"snapshots,omitempty"`
	Kept      int                  `json:"kept"`
	Taken     int                  `json:"taken"`
	Deleted   int                  `json:"deleted"`
	Failures  int                  `json:"failures"`
	RetryAt   string               `json:"retryAt,omitempty"`
}

// scheduledSnapshot is a snapshot a schedule took.
type scheduledSnapshot struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Volume      string `json:"volume"`
	Plugin      string `json:"plugin"`
	Scheduled   string `json:"scheduled"`
	CreateTime  string `json:"createTime,omitempty"`
	SizeBytes   int64  `json:"sizeBytes"`
	Ready       bool   `json:"ready"`
	DeleteError string `json:"deleteError,omitempty"`
}

// parse checks the spec and returns its cron expression, time zone and
// maximum age.
func (s *snapshotScheduleSpec) parse() (*cronexpr.Expression, *time.Location, time.Duration, error) {
	expr, err := parseCron(s.Schedule)
	if err != nil {
		return nil, nil, 0, err
	}
	tz := s.TimeZone
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("time_zone %q: %v", tz, err)
	}
	var maxAge time.Duration
	if s.Keep.MaxAge != "" {
		if maxAge, err = time.ParseDuration(s.Keep.MaxAge); err != nil || maxAge <= 0 {
			return nil, nil, 0, fmt.Errorf("keep.max_age %q is not a positive duration (for example \"720h\")", s.Keep.MaxAge)
		}
	}
	if s.Keep.Last <= 0 && maxAge == 0 {
		return nil, nil, 0, errors.New("keep needs last, max_age or both")
	}
	return expr, loc, maxAge, nil
}

// parseCron parses a cron expression; the library panics on some input.
func parseCron(spec string) (expr *cronexpr.Expression, err error) {
	defer func() {
		if recover() != nil {
			expr, err = nil, fmt.Errorf("schedule %q is not a cron expression", spec)
		}
	}()
	expr, err = cronexpr.Parse(spec)
	if err != nil {
		return nil, fmt.Errorf("schedule %q is not a cron expression: %v", spec, err)
	}
	if expr.Next(time.Now()).IsZero() {
		return nil, fmt.Errorf("schedule %q has no time in the future", spec)
	}
	return expr, nil
}

// snapshotName is the name of the snapshot a schedule takes for a time.
func snapshotName(obj *structs.CustomObject, spec *snapshotScheduleSpec, scheduled time.Time) string {
	prefix := spec.NamePrefix
	if prefix == "" {
		prefix = obj.Namespace + "-" + obj.Name
	}
	return prefix + "-" + scheduled.UTC().Format("20060102-150405")
}

// snapshotCSI is how the operator reaches the controller plugins. Tests
// replace it.
type snapshotCSI interface {
	create(snap *state.StateSnapshot, namespace string, req *structs.CSISnapshot) (*structs.CSISnapshot, error)
	delete(snap *state.StateSnapshot, req *structs.CSISnapshot) error
}

// endpointSnapshotCSI reaches the plugins as the snapshot RPCs do.
type endpointSnapshotCSI struct{ v *CSIVolume }

func (e endpointSnapshotCSI) create(snap *state.StateSnapshot, namespace string, req *structs.CSISnapshot) (*structs.CSISnapshot, error) {
	return e.v.createSnapshot(snap, namespace, req)
}

func (e endpointSnapshotCSI) delete(snap *state.StateSnapshot, req *structs.CSISnapshot) error {
	return e.v.deleteSnapshot(snap, req)
}

// snapshotOperator is the built-in operator of snapshot schedules.
type snapshotOperator struct {
	srv    *Server
	logger hclog.Logger
	csi    snapshotCSI
}

func newSnapshotOperator(srv *Server, logger hclog.Logger) *snapshotOperator {
	var csi snapshotCSI = endpointSnapshotCSI{v: NewCSIVolumeEndpoint(srv, nil)}
	if srv.config != nil && srv.config.snapshotCSI != nil {
		csi = srv.config.snapshotCSI
	}
	return &snapshotOperator{srv: srv, logger: logger.Named("snapshots"), csi: csi}
}

func (o *snapshotOperator) kinds() []*structs.ObjectKind { return builtinSnapshotKinds() }

// snapshotPass is what one pass of the operator read.
type snapshotPass struct {
	now   time.Time
	store *state.StateStore
	snap  *state.StateSnapshot
	ws    memdb.WatchSet
	wish  time.Time
}

func (p *snapshotPass) want(t time.Time) {
	if !t.IsZero() && (p.wish.IsZero() || t.Before(p.wish)) {
		p.wish = t
	}
}

func (o *snapshotOperator) reconcile(ws memdb.WatchSet) (time.Time, error) {
	store := o.srv.State()
	snap, err := store.Snapshot()
	if err != nil {
		return time.Time{}, err
	}
	p := &snapshotPass{now: time.Now().UTC(), store: store, snap: snap, ws: ws}

	objs, err := listObjects(store, ws, volumeSnapshotScheduleKind, structs.AllNamespacesSentinel)
	if err != nil {
		return time.Time{}, err
	}
	for _, obj := range objs {
		o.reconcileSchedule(p, obj)
	}
	return p.wish, nil
}

// snapshotOutcome is what a pass found out about a schedule, for its
// conditions.
type snapshotOutcome struct {
	// notReadyReason and notReadyMessage say why the schedule can't take
	// snapshots now.
	notReadyReason, notReadyMessage string

	// failedMessage is the error of the last attempt, if it failed.
	failedMessage string

	unsupported bool
}

// reconcileSchedule brings one schedule to its wanted state.
func (o *snapshotOperator) reconcileSchedule(p *snapshotPass, obj *structs.CustomObject) {
	spec := &snapshotScheduleSpec{}
	if err := structs.DecodeObjectPart(obj.Spec, spec); err != nil {
		o.logger.Warn("snapshot schedule with a spec that can't be decoded", "namespace", obj.Namespace,
			"schedule", obj.Name, "error", err)
		return
	}
	st := &snapshotScheduleStatus{}
	if err := structs.DecodeObjectPart(obj.Status, st); err != nil {
		st = &snapshotScheduleStatus{}
	}

	if obj.Deleting() {
		o.finishDeletion(p, obj, spec, st)
		return
	}

	out := &snapshotOutcome{}
	secrets, secretErr := o.secrets(obj, spec)
	expr, loc, maxAge, specErr := spec.parse()

	vol, err := p.store.CSIVolumeByID(p.ws, obj.Namespace, spec.Volume)
	if err != nil {
		o.logger.Warn("volume lookup failed", "namespace", obj.Namespace, "volume", spec.Volume, "error", err)
		p.want(p.now.Add(operatorsRetry))
		return
	}
	var plugin *structs.CSIPlugin
	if vol != nil {
		if plugin, err = p.store.CSIPluginByID(p.ws, vol.PluginID); err != nil {
			o.logger.Warn("plugin lookup failed", "plugin", vol.PluginID, "error", err)
			p.want(p.now.Add(operatorsRetry))
			return
		}
		st.Plugin = vol.PluginID
	}

	switch {
	case specErr != nil:
		out.notReadyReason, out.notReadyMessage = "InvalidSpec", specErr.Error()
	case vol == nil:
		out.notReadyReason, out.notReadyMessage = "VolumeNotFound",
			fmt.Sprintf("no CSI volume %q in the namespace %q", spec.Volume, obj.Namespace)
	case plugin == nil:
		out.notReadyReason, out.notReadyMessage = "PluginNotFound", fmt.Sprintf("no CSI plugin %q", vol.PluginID)
	case !plugin.HasControllerCapability(structs.CSIControllerSupportsCreateDeleteSnapshot):
		out.unsupported = true
		out.notReadyReason, out.notReadyMessage = "Unsupported",
			fmt.Sprintf("the controller of the CSI plugin %q can't create snapshots", vol.PluginID)
	case secretErr != nil:
		out.notReadyReason, out.notReadyMessage = "SecretUnavailable", secretErr.Error()
	}

	if specErr == nil {
		o.schedule(p, obj, spec, st, expr, loc, vol, secrets, out)
		o.refreshPending(p, obj, spec, st, vol, secrets, out)
		o.retain(p, obj, st, maxAge, spec.Keep.Last, secrets)
	}

	finalizers := slices.Clone(obj.Finalizers)
	if !slices.Contains(finalizers, structs.OperatorsFinalizer) {
		// The operator decides at the deletion whether the snapshots go
		// (delete_snapshots may change until then).
		finalizers = append(finalizers, structs.OperatorsFinalizer)
	}
	o.writeSchedule(p, obj, st, out, spec.Suspend, &finalizers)
}

// schedule takes the snapshot of the latest scheduled time that has come,
// unless it was taken.
func (o *snapshotOperator) schedule(p *snapshotPass, obj *structs.CustomObject, spec *snapshotScheduleSpec,
	st *snapshotScheduleStatus, expr *cronexpr.Expression, loc *time.Location, vol *structs.CSIVolume,
	secrets structs.CSISecrets, out *snapshotOutcome) {

	// The first time of a new schedule is the first one after the object
	// was created.
	base := time.Unix(0, obj.CreateTime).UTC()
	if last, err := time.Parse(time.RFC3339, st.LastRun); err == nil {
		base = last
	}
	due, next := scheduleTimes(expr, loc, base, p.now)
	st.NextRun = ""
	if !next.IsZero() {
		st.NextRun = next.UTC().Format(time.RFC3339)
	}
	p.want(next)

	if due.IsZero() || spec.Suspend || out.notReadyReason != "" {
		if !due.IsZero() && !spec.Suspend {
			// Taken once the schedule can take it, if no later time
			// has come by then.
			p.want(p.now.Add(operatorsRetry))
		}
		return
	}
	if retryAt, err := time.Parse(time.RFC3339, st.RetryAt); err == nil && p.now.Before(retryAt) {
		out.failedMessage = snapshotFailure(obj.Status)
		p.want(retryAt)
		return
	}

	name := snapshotName(obj, spec, due)
	created, err := o.csi.create(p.snap, obj.Namespace, &structs.CSISnapshot{
		SourceVolumeID: vol.ID,
		PluginID:       vol.PluginID,
		Name:           name,
		Secrets:        secrets,
		Parameters:     spec.Parameters,
	})
	if err != nil {
		st.Failures++
		wait := min(snapshotRetryBase<<min(st.Failures-1, 16), snapshotRetryMax)
		retryAt := p.now.Add(wait)
		st.RetryAt = retryAt.Format(time.RFC3339)
		out.failedMessage = err.Error()
		p.want(retryAt)
		operatorCount(volumeSnapshotScheduleKind, "snapshot_failures")
		o.srv.operatorEvent(obj, structs.ObjectEventTypeWarning, "SnapshotFailed",
			fmt.Sprintf("snapshot %s of the volume %s failed (attempt %d): %v", name, vol.ID, st.Failures, err))
		return
	}

	st.Failures, st.RetryAt = 0, ""
	st.LastRun = due.UTC().Format(time.RFC3339)
	entry := &scheduledSnapshot{
		ID:        created.ID,
		Name:      name,
		Volume:    vol.ID,
		Plugin:    vol.PluginID,
		Scheduled: st.LastRun,
		SizeBytes: created.SizeBytes,
		Ready:     created.IsReady,
	}
	if created.CreateTime > 0 {
		entry.CreateTime = time.Unix(created.CreateTime, 0).UTC().Format(time.RFC3339)
	}
	// The same snapshot again (a leader that took over): update it.
	st.Snapshots = slices.DeleteFunc(st.Snapshots, func(s *scheduledSnapshot) bool { return s.ID == created.ID })
	st.Snapshots = append(st.Snapshots, entry)
	st.Taken++
	operatorCount(volumeSnapshotScheduleKind, "snapshots_taken")
	o.srv.operatorEvent(obj, structs.ObjectEventTypeNormal, "SnapshotTaken",
		fmt.Sprintf("snapshot %s (%s) of the volume %s", name, created.ID, vol.ID))

	// The next time after the one just taken.
	if _, next := scheduleTimes(expr, loc, due, p.now); !next.IsZero() {
		st.NextRun = next.UTC().Format(time.RFC3339)
		p.want(next)
	}
}

// scheduleTimes returns the latest time of the schedule after base that has
// come by now (zero if none has), and the first one after now.
func scheduleTimes(expr *cronexpr.Expression, loc *time.Location, base, now time.Time) (due, next time.Time) {
	// A schedule that was not run for a long time: only the last year
	// counts, which bounds the search.
	if floor := now.AddDate(-1, 0, 0); base.Before(floor) {
		base = floor
	}
	t := expr.Next(base.In(loc))
	for !t.IsZero() && !t.After(now) {
		due = t
		t = expr.Next(t)
	}
	return due, t
}

// refreshPending asks the plugin again for the snapshots it has not
// finished: CSI answers a repeated request of the same name with the
// snapshot's current state.
func (o *snapshotOperator) refreshPending(p *snapshotPass, obj *structs.CustomObject, spec *snapshotScheduleSpec,
	st *snapshotScheduleStatus, vol *structs.CSIVolume, secrets structs.CSISecrets, out *snapshotOutcome) {

	if vol == nil || out.notReadyReason != "" {
		return
	}
	for _, s := range st.Snapshots {
		if s.Ready || s.Volume != vol.ID {
			continue
		}
		got, err := o.csi.create(p.snap, obj.Namespace, &structs.CSISnapshot{
			SourceVolumeID: vol.ID,
			PluginID:       vol.PluginID,
			Name:           s.Name,
			Secrets:        secrets,
			Parameters:     spec.Parameters,
		})
		if err != nil {
			o.logger.Debug("asking for an unfinished snapshot failed", "snapshot", s.Name, "error", err)
			p.want(p.now.Add(snapshotReadyPoll))
			continue
		}
		s.Ready, s.SizeBytes = got.IsReady, got.SizeBytes
		if got.CreateTime > 0 {
			s.CreateTime = time.Unix(got.CreateTime, 0).UTC().Format(time.RFC3339)
		}
		if !s.Ready {
			p.want(p.now.Add(snapshotReadyPoll))
		}
	}
}

// retain deletes the snapshots of the schedule its retention rule lets go:
// one is kept while it is among the last newest, or younger than maxAge.
// The newest finished snapshot is always kept, and one that is not finished
// is never deleted.
func (o *snapshotOperator) retain(p *snapshotPass, obj *structs.CustomObject, st *snapshotScheduleStatus,
	maxAge time.Duration, last int, secrets structs.CSISecrets) {

	sort.SliceStable(st.Snapshots, func(i, j int) bool { return st.Snapshots[i].Scheduled > st.Snapshots[j].Scheduled })
	newestReady := slices.IndexFunc(st.Snapshots, func(s *scheduledSnapshot) bool { return s.Ready })

	var keep []*scheduledSnapshot
	for i, s := range st.Snapshots {
		scheduled, _ := time.Parse(time.RFC3339, s.Scheduled)
		kept := !s.Ready || i == newestReady ||
			(last > 0 && i < last) ||
			(maxAge > 0 && p.now.Sub(scheduled) < maxAge)
		if kept {
			keep = append(keep, s)
			if maxAge > 0 {
				p.want(scheduled.Add(maxAge))
			}
			continue
		}
		if err := o.deleteSnapshot(p, s, secrets); err != nil {
			s.DeleteError = err.Error()
			keep = append(keep, s)
			p.want(p.now.Add(operatorsRetry))
			o.srv.operatorEvent(obj, structs.ObjectEventTypeWarning, "SnapshotDeleteFailed",
				fmt.Sprintf("snapshot %s (%s) could not be deleted: %v", s.Name, s.ID, err))
			continue
		}
		st.Deleted++
		operatorCount(volumeSnapshotScheduleKind, "snapshots_deleted")
		o.srv.operatorEvent(obj, structs.ObjectEventTypeNormal, "SnapshotDeleted",
			fmt.Sprintf("snapshot %s (%s) is past the retention", s.Name, s.ID))
	}
	st.Snapshots = keep
}

func (o *snapshotOperator) deleteSnapshot(p *snapshotPass, s *scheduledSnapshot, secrets structs.CSISecrets) error {
	return o.csi.delete(p.snap, &structs.CSISnapshot{ID: s.ID, PluginID: s.Plugin, Secrets: secrets})
}

// secrets reads the CSI secrets of a schedule from the secret store.
func (o *snapshotOperator) secrets(obj *structs.CustomObject, spec *snapshotScheduleSpec) (structs.CSISecrets, error) {
	if spec.SecretPath == "" {
		return nil, nil
	}
	items, _, err := o.srv.secretReadValue(obj.Namespace, spec.SecretPath, 0)
	if err != nil {
		return nil, fmt.Errorf("the secret %q can't be read: %v", spec.SecretPath, err)
	}
	return structs.CSISecrets(items), nil
}

// snapshotFailure returns the message of the Failed condition of a status,
// if it is true.
func snapshotFailure(status map[string]any) string {
	for _, c := range operatorConditions(status) {
		if c.Type == snapshotConditionFailed && c.Status == "True" {
			return c.Message
		}
	}
	return ""
}

// writeSchedule writes the status of a schedule with its conditions.
func (o *snapshotOperator) writeSchedule(p *snapshotPass, obj *structs.CustomObject, st *snapshotScheduleStatus,
	out *snapshotOutcome, suspended bool, finalizers *[]string) *structs.CustomObject {

	st.Kept = len(st.Snapshots)
	encoded, err := structs.EncodeObjectPart(st)
	if err != nil {
		o.logger.Warn("status of a snapshot schedule can't be encoded", "schedule", obj.Name, "error", err)
		return obj
	}
	conditions := newConditionSet(obj.Status, p.now)
	ready := out.notReadyReason == "" && out.failedMessage == ""
	reason, message := out.notReadyReason, out.notReadyMessage
	if reason == "" && out.failedMessage != "" {
		reason, message = "SnapshotFailed", out.failedMessage
	}
	conditions.set(structs.CertificateConditionReady, ready, reason, message)
	conditions.set(snapshotConditionFailed, out.failedMessage != "", "", out.failedMessage)
	conditions.set(snapshotConditionUnsupported, out.unsupported, "", "")
	conditions.set(snapshotConditionSuspended, suspended, "", "")
	conditions.set(structs.CertificateConditionDegraded, !ready, "", "")
	encoded[structs.ObjectStatusConditions] = conditions.list()
	encoded[structs.ObjectStatusObservedGeneration] = obj.Generation
	return o.writeStatus(obj, encoded, finalizers)
}

// writeStatus writes a changed status and finalizers of a schedule.
func (o *snapshotOperator) writeStatus(obj *structs.CustomObject, status map[string]any, finalizers *[]string) *structs.CustomObject {
	normalized, err := structs.NormalizeObjectMap(status)
	if err != nil {
		o.logger.Warn("status of a snapshot schedule can't be encoded", "schedule", obj.Name, "error", err)
		return obj
	}
	if structs.ObjectMapsEqual(obj.Status, normalized) && (finalizers == nil || slices.Equal(obj.Finalizers, *finalizers)) {
		return obj
	}
	changed := obj.Copy()
	changed.Status = normalized
	if finalizers != nil {
		changed.Finalizers = slices.Clone(*finalizers)
	}
	written, err := o.srv.writeOperatorObject(changed)
	if err != nil {
		if !structs.IsObjectConflictError(err) {
			o.logger.Warn("failed to write the status of a snapshot schedule", "schedule", obj.Name,
				"namespace", obj.Namespace, "error", err)
		}
		return obj
	}
	return written
}

// finishDeletion does what the deletion of a schedule asks of the operator:
// with delete_snapshots it deletes the snapshots the schedule took, then it
// lets the object go. A snapshot that can't be deleted holds the object, and
// is tried again.
func (o *snapshotOperator) finishDeletion(p *snapshotPass, obj *structs.CustomObject, spec *snapshotScheduleSpec, st *snapshotScheduleStatus) {
	if !slices.Contains(obj.Finalizers, structs.OperatorsFinalizer) {
		return
	}
	if spec.DeleteSnapshots && len(st.Snapshots) > 0 {
		secrets, err := o.secrets(obj, spec)
		var failed []string
		var left []*scheduledSnapshot
		for _, s := range st.Snapshots {
			if err == nil {
				err = o.deleteSnapshot(p, s, secrets)
			}
			if err != nil {
				s.DeleteError = err.Error()
				left = append(left, s)
				failed = append(failed, s.Name)
				err = nil
				continue
			}
			st.Deleted++
			operatorCount(volumeSnapshotScheduleKind, "snapshots_deleted")
		}
		if len(left) > 0 {
			st.Snapshots = left
			p.want(p.now.Add(operatorsRetry))
			out := &snapshotOutcome{notReadyReason: "Deleting",
				notReadyMessage: "snapshots that could not be deleted yet: " + strings.Join(failed, ", ")}
			o.writeSchedule(p, obj, st, out, spec.Suspend, nil)
			return
		}
	}
	changed := obj.Copy()
	changed.Finalizers = slices.DeleteFunc(changed.Finalizers, func(f string) bool { return f == structs.OperatorsFinalizer })
	written, err := o.srv.writeOperatorObject(changed)
	if err != nil {
		if !structs.IsObjectConflictError(err) {
			o.logger.Warn("failed to remove the finalizer of a deleted snapshot schedule", "schedule", obj.Name, "error", err)
		}
		return
	}
	o.srv.finalizeObjectIfReady(written)
}

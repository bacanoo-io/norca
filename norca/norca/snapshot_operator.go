// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
// The name of a snapshot is derived from the time it was scheduled for, and
// it is written into the status before the plugin is asked for it. CSI
// creates a snapshot of a name once and answers again with the one it has,
// so a leader that takes over a snapshot in progress, or tries a failed one
// again, asks for the name in the status and never makes a second one, and
// no snapshot the plugin made is left out of the list: not when the answer
// of the plugin was lost, and not when the leader changed before it could
// be written down. A time missed while there was no leader is taken once
// when the next leader comes, not once per missed time.
//
// Snapshots are crash-consistent: what the volume holds at the moment of the
// snapshot, without any help from the application.

const (
	// snapshotRetryBase and snapshotRetryMax bound the wait after a failed
	// snapshot: it doubles with every failure in a row. A scheduled time
	// that comes earlier is tried at its time.
	snapshotRetryBase = time.Minute
	snapshotRetryMax  = time.Hour

	// snapshotReadyPoll is how often a snapshot the plugin has not finished
	// is asked for again.
	snapshotReadyPoll = 30 * time.Second

	// snapshotPassBudget is how long a pass goes on beginning with
	// another schedule. The calls of the plugins are made in the loop all
	// the built-in operators share, and one may take minutes with a plugin
	// that does not answer: a pass that has used its budget leaves the
	// schedules it has not come to for the next one, which begins with
	// them, and the other operators run in between.
	snapshotPassBudget = 20 * time.Second
)

// snapshotTuning is implemented by the plugin of a test that changes the
// waits of the operator of its server.
type snapshotTuning interface {
	tuning() (retryBase, readyPoll, passBudget time.Duration)
}

// The snapshots of a schedule are listed in its status, and an object is at
// most structs.MaxCustomObjectSize of JSON. A status that could not be
// written would stop the schedule from recording anything, so the list is
// bounded where it grows.
const (
	// snapshotMaxKept is how many snapshots the retention of a schedule
	// may keep: the maximum of keep.last in the schema, and what max_age
	// may keep at the rate of the schedule (checked when it is written).
	snapshotMaxKept = 500

	// snapshotStatusBudget is the size of the spec and status up to which
	// another snapshot is begun. Past it the schedule takes nothing
	// (reason StatusFull) until its retention has made room: the rest is
	// left for what is written about the snapshots that are listed.
	snapshotStatusBudget = structs.MaxCustomObjectSize * 3 / 4

	// snapshotStatusLimit is the size past which the errors of failed
	// deletions are cut down to snapshotDeleteErrorShort; the conditions
	// of the status take the rest.
	snapshotStatusLimit = structs.MaxCustomObjectSize - 16*1024

	// snapshotDeleteErrorMax is how many bytes of the error of a failed
	// deletion the status keeps for a snapshot.
	snapshotDeleteErrorMax = 200

	snapshotDeleteErrorShort = "failed"
)

// snapshotErrorText returns the error of a failed deletion as the status
// keeps it.
func snapshotErrorText(err error) string {
	text := err.Error()
	if len(text) > snapshotDeleteErrorMax {
		text = strings.ToValidUTF8(text[:snapshotDeleteErrorMax], "")
	}
	return text
}

// snapshotStatusSize returns the size of the spec of a schedule with a
// status, without the conditions, as the size limit of objects counts it.
func snapshotStatusSize(obj *structs.CustomObject, st *snapshotScheduleStatus) int {
	b, err := json.Marshal(map[string]any{"spec": obj.Spec, "status": st})
	if err != nil {
		return structs.MaxCustomObjectSize
	}
	return len(b)
}

// snapshotFitStatus makes a status fit the size limit of objects: the list
// of snapshots is never cut (it is the only record of them), the errors of
// failed deletions are.
func snapshotFitStatus(obj *structs.CustomObject, st *snapshotScheduleStatus) {
	if snapshotStatusSize(obj, st) <= snapshotStatusLimit {
		return
	}
	for _, s := range st.Snapshots {
		if s.DeleteError != "" {
			s.DeleteError = snapshotDeleteErrorShort
		}
	}
}

// snapshotTimesWindow is the stretch of a schedule that is walked time by
// time to find the latest one that has come.
const snapshotTimesWindow = time.Minute

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

	// FailedRun is the latest scheduled time that had come when the last
	// attempt failed: RetryAt holds only until a later time comes.
	FailedRun string `json:"failedRun,omitempty"`

	// DeleteFailures counts the passes in a row in which a deletion
	// failed; no deletion is tried before DeleteRetryAt.
	DeleteFailures int    `json:"deleteFailures,omitempty"`
	DeleteRetryAt  string `json:"deleteRetryAt,omitempty"`
}

// scheduledSnapshot is a snapshot a schedule took, or one it has begun: the
// plugin has not answered for a snapshot without an ID yet, and is asked
// for it by its name until it does.
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
// maximum age. A schedule that has ended (one with a year field) is valid
// here: its last time is still to be taken and its snapshots are still to be
// retained; only writing one is refused (checkSnapshotApply).
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
	return expr, nil
}

// snapshotPrefixMax is how much of the prefix of a snapshot name is used.
// With the ID part and the time a name is at most 89 bytes: CSI limits its
// strings to 128.
const snapshotPrefixMax = 64

// snapshotName is the name of the snapshot a schedule takes for a time:
// <prefix>-<the first 8 hex digits of the object ID>-<the time in UTC>.
// The part of the ID keeps the names of two schedules apart, whatever
// their prefixes are: a plugin makes a snapshot of a name once, so two
// schedules with one name would share, and delete, each other's snapshot.
func snapshotName(obj *structs.CustomObject, spec *snapshotScheduleSpec, scheduled time.Time) string {
	prefix := spec.NamePrefix
	if prefix == "" {
		prefix = obj.Namespace + "-" + obj.Name
	}
	if len(prefix) > snapshotPrefixMax {
		prefix = prefix[:snapshotPrefixMax]
	}
	id := strings.ReplaceAll(obj.ID, "-", "")
	if len(id) > 8 {
		id = id[:8]
	}
	return prefix + "-" + id + "-" + scheduled.UTC().Format("20060102-150405")
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

	retryBase  time.Duration
	readyPoll  time.Duration
	passBudget time.Duration

	// resume is the schedule the next pass begins with: the first one
	// the last pass did not come to.
	resume string
}

func newSnapshotOperator(srv *Server, logger hclog.Logger) *snapshotOperator {
	var csi snapshotCSI = endpointSnapshotCSI{v: NewCSIVolumeEndpoint(srv, nil)}
	if srv.config != nil && srv.config.snapshotCSI != nil {
		csi = srv.config.snapshotCSI
	}
	o := &snapshotOperator{srv: srv, logger: logger.Named("snapshots"), csi: csi,
		retryBase: snapshotRetryBase, readyPoll: snapshotReadyPoll, passBudget: snapshotPassBudget}
	if t, ok := csi.(snapshotTuning); ok {
		o.retryBase, o.readyPoll, o.passBudget = t.tuning()
	}
	return o
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
	// The schedules the last pass did not come to are the first ones.
	key := func(obj *structs.CustomObject) string { return obj.Namespace + "/" + obj.Name }
	if at := slices.IndexFunc(objs, func(obj *structs.CustomObject) bool { return key(obj) == o.resume }); at > 0 {
		objs = append(slices.Clone(objs[at:]), objs[:at]...)
	}
	o.resume = ""
	started := time.Now()
	for _, obj := range objs {
		// A server that is no longer the leader makes no more calls;
		// a pass out of its budget goes on in the next one.
		if !o.srv.IsLeader() {
			break
		}
		if time.Since(started) > o.passBudget {
			o.resume = key(obj)
			p.want(time.Now().Add(operatorsSettle))
			break
		}
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

// snapshotRun is one schedule in a pass.
type snapshotRun struct {
	// obj is the object as it was read, or as the pass wrote it last.
	obj     *structs.CustomObject
	spec    *snapshotScheduleSpec
	st      *snapshotScheduleStatus
	out     *snapshotOutcome
	vol     *structs.CSIVolume
	secrets structs.CSISecrets

	// due is the latest time of the schedule that has come since the
	// last run (zero: none).
	due time.Time

	// full is set when a snapshot is due and the status has no room for
	// it.
	full bool
}

// latestRun returns the latest scheduled time that has come, as the status
// has it.
func (r *snapshotRun) latestRun() string {
	if r.due.IsZero() {
		return r.st.LastRun
	}
	return r.due.UTC().Format(time.RFC3339)
}

// withOperatorsFinalizer returns the finalizers of a schedule with the one
// of the operators: the operator decides at the deletion whether the
// snapshots go (delete_snapshots may change until then).
func withOperatorsFinalizer(finalizers []string) []string {
	out := slices.Clone(finalizers)
	if !slices.Contains(out, structs.OperatorsFinalizer) {
		out = append(out, structs.OperatorsFinalizer)
	}
	return out
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
	r := &snapshotRun{obj: obj, spec: spec, st: st, out: out, secrets: secrets}

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
	r.vol = vol

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
		o.schedule(p, r, expr, loc)
		o.finish(p, r)
		if r.full && out.notReadyReason == "" {
			out.notReadyReason, out.notReadyMessage = "StatusFull", fmt.Sprintf(
				"the status lists %d snapshots and has no room for another one: no snapshot is taken until "+
					"the retention has deleted some (a shorter keep makes room)", len(st.Snapshots))
		}
		if secretErr == nil {
			o.retain(p, r.obj, st, maxAge, spec.Keep.Last, secrets)
		} else if len(st.Snapshots) > 0 {
			// A deletion without the secret would be refused by a
			// plugin that needs it, time after time: the retention
			// waits for the secret. Nothing tells when it is there.
			p.want(p.now.Add(operatorsRetry))
		}
	}

	finalizers := withOperatorsFinalizer(r.obj.Finalizers)
	o.writeSchedule(p, r.obj, st, out, spec.Suspend, &finalizers)
}

// schedule begins the snapshot of the latest scheduled time that has come,
// unless it was begun: it writes the snapshot into the status, by the name
// the plugin is asked for. Only a snapshot that is written down is asked
// for (in finish), so whatever happens to the answer, the list has it.
func (o *snapshotOperator) schedule(p *snapshotPass, r *snapshotRun, expr *cronexpr.Expression, loc *time.Location) {
	st := r.st

	// The first time of a new schedule is the first one after the object
	// was created.
	base := time.Unix(0, r.obj.CreateTime).UTC()
	if last, err := time.Parse(time.RFC3339, st.LastRun); err == nil {
		base = last
	}
	due, next := scheduleTimes(expr, loc, base, p.now)
	st.NextRun = ""
	if !next.IsZero() {
		st.NextRun = next.UTC().Format(time.RFC3339)
	}
	p.want(next)
	if due.IsZero() {
		return
	}
	r.due = due

	if r.spec.Suspend || r.out.notReadyReason != "" {
		if !r.spec.Suspend {
			// Taken once the schedule can take it, if no later time
			// has come by then.
			p.want(p.now.Add(operatorsRetry))
		}
		return
	}
	// One at a time: a snapshot the plugin has not answered for is asked
	// for again under its name, and stands for the times that come until
	// it is made. A new name now would leave it out of the list if the
	// plugin made it after all.
	if slices.ContainsFunc(st.Snapshots, func(s *scheduledSnapshot) bool { return s.ID == "" }) {
		return
	}

	// Nor is one begun that the status has no room for: a snapshot that
	// can't be written down is not made. The retention makes room.
	if snapshotStatusSize(r.obj, st) > snapshotStatusBudget {
		r.full = true
		p.want(p.now.Add(operatorsRetry))
		return
	}

	before := *st
	name := snapshotName(r.obj, r.spec, due)
	st.LastRun = due.UTC().Format(time.RFC3339)
	st.Failures, st.RetryAt, st.FailedRun = 0, "", ""
	if !slices.ContainsFunc(st.Snapshots, func(s *scheduledSnapshot) bool { return s.Name == name }) {
		st.Snapshots = append(slices.Clone(st.Snapshots), &scheduledSnapshot{
			Name:      name,
			Volume:    r.vol.ID,
			Plugin:    r.vol.PluginID,
			Scheduled: st.LastRun,
		})
	}
	written, err := o.writeProgress(r)
	if err != nil {
		// Not written down: not asked for. The next pass begins it
		// again.
		*st = before
		p.want(p.now.Add(operatorsRetry))
		return
	}
	r.obj = written
}

// scheduleTimes returns the latest time of the schedule after base that has
// come by now (zero if none has), and the first one after now.
func scheduleTimes(expr *cronexpr.Expression, loc *time.Location, base, now time.Time) (due, next time.Time) {
	// A schedule that was not run for a long time: only the last year
	// counts, which bounds the search.
	if floor := now.AddDate(-1, 0, 0); base.Before(floor) {
		base = floor
	}
	lo := base.In(loc)
	t := expr.Next(lo)
	if t.IsZero() || t.After(now) {
		return time.Time{}, t
	}
	// A time has come since base. The walk below costs a step for every
	// time it passes, which is a year of them for a schedule that was
	// suspended or not ready that long: start it later, at a point that
	// still has a time between it and now. Halving finds one at most
	// snapshotTimesWindow before the latest time in a few steps.
	for hi := now; hi.Sub(lo) > snapshotTimesWindow; {
		mid := lo.Add(hi.Sub(lo) / 2)
		if m := expr.Next(mid); !m.IsZero() && !m.After(now) {
			lo, t = mid, m
		} else {
			hi = mid
		}
	}
	for !t.IsZero() && !t.After(now) {
		due = t
		t = expr.Next(t)
	}
	return due, t
}

// finish asks the plugin for the snapshots of the status it has not
// finished: for one that was begun and has no answer yet, and again for one
// the plugin was still making. CSI answers a repeated request of the same
// name with the snapshot's current state.
func (o *snapshotOperator) finish(p *snapshotPass, r *snapshotRun) {
	if r.vol == nil || r.out.notReadyReason != "" {
		return
	}
	st := r.st
	for _, s := range st.Snapshots {
		if s.Ready || s.Volume != r.vol.ID {
			continue
		}
		answered := s.ID != ""
		if !answered {
			if r.spec.Suspend {
				continue
			}
			// The wait after a failure holds until a later scheduled
			// time comes: that one is tried at its time.
			retryAt, err := time.Parse(time.RFC3339, st.RetryAt)
			if err == nil && p.now.Before(retryAt) && st.FailedRun == r.latestRun() {
				r.out.failedMessage = snapshotFailure(r.obj.Status)
				p.want(retryAt)
				continue
			}
		}

		got, err := o.csi.create(p.snap, r.obj.Namespace, &structs.CSISnapshot{
			SourceVolumeID: r.vol.ID,
			PluginID:       r.vol.PluginID,
			Name:           s.Name,
			Secrets:        r.secrets,
			Parameters:     r.spec.Parameters,
		})
		if err == nil && got.ID == "" {
			err = errors.New("the plugin answered with a snapshot that has no ID")
		}
		switch {
		case err != nil && answered:
			// The plugin refuses to say how the snapshot is (it does
			// when the parameters changed meanwhile, for one): the
			// schedule says so instead of asking silently for ever.
			r.out.failedMessage = fmt.Sprintf("snapshot %s (%s) is not finished, and asking the plugin for it failed: %v",
				s.Name, s.ID, err)
			o.logger.Debug("asking for an unfinished snapshot failed", "snapshot", s.Name, "error", err)
			p.want(p.now.Add(o.readyPoll))
			continue
		case err != nil:
			st.Failures++
			wait := min(o.retryBase<<min(st.Failures-1, 16), snapshotRetryMax)
			retryAt := p.now.Add(wait)
			st.RetryAt = retryAt.Format(time.RFC3339)
			st.FailedRun = r.latestRun()
			r.out.failedMessage = err.Error()
			p.want(retryAt)
			operatorCount(volumeSnapshotScheduleKind, "snapshot_failures")
			o.srv.operatorEvent(r.obj, structs.ObjectEventTypeWarning, "SnapshotFailed",
				fmt.Sprintf("snapshot %s of the volume %s failed (attempt %d): %v", s.Name, r.vol.ID, st.Failures, err))
			continue
		}

		switch {
		case !answered:
			st.Failures, st.RetryAt, st.FailedRun = 0, "", ""
			// It stands for the times that came while it could not be
			// made.
			st.LastRun = max(st.LastRun, r.latestRun())
			st.Taken++
			operatorCount(volumeSnapshotScheduleKind, "snapshots_taken")
			o.srv.operatorEvent(r.obj, structs.ObjectEventTypeNormal, "SnapshotTaken",
				fmt.Sprintf("snapshot %s (%s) of the volume %s", s.Name, got.ID, r.vol.ID))
		case got.ID != s.ID:
			// The snapshot that was being made is gone and the plugin
			// made the name again: the list follows the one that
			// exists.
			o.srv.operatorEvent(r.obj, structs.ObjectEventTypeWarning, "SnapshotReplaced",
				fmt.Sprintf("snapshot %s was %s and is %s now: the plugin made it again", s.Name, s.ID, got.ID))
		}
		s.ID, s.Ready, s.SizeBytes = got.ID, got.IsReady, got.SizeBytes
		if got.CreateTime > 0 {
			s.CreateTime = time.Unix(got.CreateTime, 0).UTC().Format(time.RFC3339)
		}
		if !s.Ready {
			p.want(p.now.Add(o.readyPoll))
		}
	}
}

// deletionsWait reports whether the deletions of a schedule wait for the
// time the status sets after failed ones.
func deletionsWait(p *snapshotPass, st *snapshotScheduleStatus) (time.Time, bool) {
	retryAt, err := time.Parse(time.RFC3339, st.DeleteRetryAt)
	return retryAt, err == nil && p.now.Before(retryAt)
}

// deletionsTried records how the deletions of a pass went: after a failed
// one the next deletions of the schedule wait, a wait that doubles with
// every pass that fails in a row, as after a failed snapshot.
func (o *snapshotOperator) deletionsTried(p *snapshotPass, st *snapshotScheduleStatus, failed bool) {
	if !failed {
		st.DeleteFailures, st.DeleteRetryAt = 0, ""
		return
	}
	st.DeleteFailures++
	retryAt := p.now.Add(min(o.retryBase<<min(st.DeleteFailures-1, 16), snapshotRetryMax))
	st.DeleteRetryAt = retryAt.Format(time.RFC3339)
	p.want(retryAt)
}

// retain deletes the snapshots of the schedule its retention rule lets go:
// one is kept while it is among the last newest, or younger than maxAge.
// The newest finished snapshot is always kept, and one that is not finished
// is never deleted. A deletion that fails is in the status (deleteError) and
// is one event, when it fails first; it is tried again after a growing wait.
func (o *snapshotOperator) retain(p *snapshotPass, obj *structs.CustomObject, st *snapshotScheduleStatus,
	maxAge time.Duration, last int, secrets structs.CSISecrets) {

	sort.SliceStable(st.Snapshots, func(i, j int) bool { return st.Snapshots[i].Scheduled > st.Snapshots[j].Scheduled })
	newestReady := slices.IndexFunc(st.Snapshots, func(s *scheduledSnapshot) bool { return s.Ready })

	retryAt, waiting := deletionsWait(p, st)
	tried, failed := false, false
	var keep []*scheduledSnapshot
	for i, s := range st.Snapshots {
		scheduled, _ := time.Parse(time.RFC3339, s.Scheduled)
		kept := !s.Ready || i == newestReady ||
			(last > 0 && i < last) ||
			(maxAge > 0 && p.now.Sub(scheduled) < maxAge)
		if kept {
			keep = append(keep, s)
			s.DeleteError = ""
			// The time it ages out, unless another rule keeps it past
			// that: a time in the past would run the loop without end.
			if agesOut := scheduled.Add(maxAge); maxAge > 0 && agesOut.After(p.now) {
				p.want(agesOut)
			}
			continue
		}
		if waiting {
			keep = append(keep, s)
			p.want(retryAt)
			continue
		}
		tried = true
		if err := o.deleteSnapshot(p, obj.Namespace, s, secrets); err != nil {
			if s.DeleteError == "" {
				o.srv.operatorEvent(obj, structs.ObjectEventTypeWarning, "SnapshotDeleteFailed",
					fmt.Sprintf("snapshot %s (%s) could not be deleted: %v", s.Name, s.ID, err))
			}
			s.DeleteError = snapshotErrorText(err)
			keep = append(keep, s)
			failed = true
			continue
		}
		st.Deleted++
		operatorCount(volumeSnapshotScheduleKind, "snapshots_deleted")
		o.srv.operatorEvent(obj, structs.ObjectEventTypeNormal, "SnapshotDeleted",
			fmt.Sprintf("snapshot %s (%s) is past the retention", s.Name, s.ID))
	}
	st.Snapshots = keep
	if tried {
		o.deletionsTried(p, st, failed)
	}
}

// deleteSnapshot asks the plugin to delete a snapshot of a schedule of the
// namespace. The plugin gets what it got when the snapshot was made: the
// secrets of the volume, if it is still there, and those of secret_path over
// them. A plugin that needs the secrets of the volume to delete would refuse
// every deletion otherwise.
func (o *snapshotOperator) deleteSnapshot(p *snapshotPass, namespace string, s *scheduledSnapshot, secrets structs.CSISecrets) error {
	vol, err := p.snap.CSIVolumeByID(nil, namespace, s.Volume)
	if err != nil {
		return fmt.Errorf("volume lookup failed: %v", err)
	}
	if vol != nil && len(vol.Secrets) > 0 {
		merged := make(structs.CSISecrets, len(vol.Secrets)+len(secrets))
		maps.Copy(merged, vol.Secrets)
		maps.Copy(merged, secrets)
		secrets = merged
	}
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
	snapshotFitStatus(obj, st)
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
	written, _ := o.writeStatus(obj, encoded, finalizers)
	return written
}

// writeProgress writes the status of a schedule in the middle of a pass,
// with the conditions as they are: the end of the pass sets them.
func (o *snapshotOperator) writeProgress(r *snapshotRun) (*structs.CustomObject, error) {
	r.st.Kept = len(r.st.Snapshots)
	encoded, err := structs.EncodeObjectPart(r.st)
	if err != nil {
		o.logger.Warn("status of a snapshot schedule can't be encoded", "schedule", r.obj.Name, "error", err)
		return nil, err
	}
	for _, key := range []string{structs.ObjectStatusConditions, structs.ObjectStatusObservedGeneration} {
		if v, ok := r.obj.Status[key]; ok {
			encoded[key] = v
		}
	}
	finalizers := withOperatorsFinalizer(r.obj.Finalizers)
	return o.writeStatus(r.obj, encoded, &finalizers)
}

// writeStatus writes a changed status and finalizers of a schedule, and
// returns the object as it is stored then. With an error nothing was
// written.
func (o *snapshotOperator) writeStatus(obj *structs.CustomObject, status map[string]any, finalizers *[]string) (*structs.CustomObject, error) {
	normalized, err := structs.NormalizeObjectMap(status)
	if err != nil {
		o.logger.Warn("status of a snapshot schedule can't be encoded", "schedule", obj.Name, "error", err)
		return obj, err
	}
	if structs.ObjectMapsEqual(obj.Status, normalized) && (finalizers == nil || slices.Equal(obj.Finalizers, *finalizers)) {
		return obj, nil
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
		return obj, err
	}
	return written, nil
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
		// While the secret can't be read, no snapshot is sent to the
		// plugin: every one of them waits for it.
		secrets, secretErr := o.secrets(obj, spec)
		retryAt, waiting := deletionsWait(p, st)
		tried, pluginFailed := false, false
		var failed []string
		var left []*scheduledSnapshot
		for _, s := range st.Snapshots {
			if s.ID == "" {
				// The plugin never answered for it: there is no ID to
				// delete it by.
				o.logger.Warn("a snapshot the plugin did not answer for is left when its schedule is deleted",
					"schedule", obj.Name, "namespace", obj.Namespace, "snapshot", s.Name)
				continue
			}
			err := secretErr
			switch {
			case err != nil:
				p.want(p.now.Add(operatorsRetry))
			case waiting:
				// Not tried before the wait after the failed ones ends.
				left = append(left, s)
				failed = append(failed, s.Name)
				p.want(retryAt)
				continue
			default:
				tried = true
				if err = o.deleteSnapshot(p, obj.Namespace, s, secrets); err != nil {
					pluginFailed = true
				}
			}
			if err != nil {
				s.DeleteError = snapshotErrorText(err)
				left = append(left, s)
				failed = append(failed, s.Name)
				continue
			}
			st.Deleted++
			operatorCount(volumeSnapshotScheduleKind, "snapshots_deleted")
		}
		if tried {
			o.deletionsTried(p, st, pluginFailed)
		}
		if len(left) > 0 {
			st.Snapshots = left
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

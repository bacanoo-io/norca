// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/hashicorp/go-memdb"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// fakeSnapshotCSI is a controller plugin of a test: a snapshot of a name is
// created once, as CSI asks.
type fakeSnapshotCSI struct {
	mu      sync.Mutex
	byName  map[string]*structs.CSISnapshot
	creates []string
	deletes []string
	secrets []structs.CSISecrets
	// deleteSecrets are the secrets of every deletion asked for.
	deleteSecrets []structs.CSISecrets
	createErr     error
	deleteErr     error
	notReady      bool

	// lostAnswers is how many creations the plugin makes while the caller
	// gets an error, as when the answer is lost on the way.
	lostAnswers int

	// retryBase, readyPoll and passBudget are the waits of the operator of
	// the test's server, if set: see tuning.
	retryBase  time.Duration
	readyPoll  time.Duration
	passBudget time.Duration

	// createDelay is how long the plugin takes to answer a creation, and
	// createPasses the pass of the operator each creation was asked in.
	createDelay  time.Duration
	createPasses []*state.StateSnapshot

	// createBlock, if set, holds every creation until it is closed, as a
	// plugin that does not answer; createEntered is told when one waits.
	createBlock   chan struct{}
	createEntered chan struct{}

	// onDelete, when set, runs in every deletion, as something that
	// happens while the plugin works.
	onDelete func()
}

// tuning sets the waits of the operator: it is read once, when the server
// of the test makes its operator.
func (f *fakeSnapshotCSI) tuning() (time.Duration, time.Duration, time.Duration) {
	retryBase, readyPoll, passBudget := snapshotRetryBase, snapshotReadyPoll, snapshotPassBudget
	if f.passBudget > 0 {
		passBudget = f.passBudget
	}
	if f.retryBase > 0 {
		retryBase = f.retryBase
	}
	if f.readyPoll > 0 {
		readyPoll = f.readyPoll
	}
	return retryBase, readyPoll, passBudget
}

// names returns the names of the snapshots the plugin holds, sorted.
func (f *fakeSnapshotCSI) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for name := range f.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func newFakeSnapshotCSI() *fakeSnapshotCSI {
	return &fakeSnapshotCSI{byName: map[string]*structs.CSISnapshot{}}
}

func (f *fakeSnapshotCSI) create(pass *state.StateSnapshot, _ string, req *structs.CSISnapshot) (*structs.CSISnapshot, error) {
	f.mu.Lock()
	delay, block, entered := f.createDelay, f.createBlock, f.createEntered
	f.mu.Unlock()
	time.Sleep(delay)
	if block != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-block
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.createPasses = append(f.createPasses, pass)
	f.creates = append(f.creates, req.Name)
	f.secrets = append(f.secrets, req.Secrets)
	if f.createErr != nil {
		return nil, f.createErr
	}
	snap, ok := f.byName[req.Name]
	if !ok {
		snap = &structs.CSISnapshot{ID: "snap-" + req.Name, SizeBytes: 42, CreateTime: time.Now().Unix()}
		f.byName[req.Name] = snap
	}
	snap.IsReady = !f.notReady
	if f.lostAnswers > 0 {
		f.lostAnswers--
		return nil, errors.New("context deadline exceeded")
	}
	out := *snap
	return &out, nil
}

func (f *fakeSnapshotCSI) delete(_ *state.StateSnapshot, req *structs.CSISnapshot) error {
	f.mu.Lock()
	onDelete := f.onDelete
	f.mu.Unlock()
	if onDelete != nil {
		onDelete()
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteSecrets = append(f.deleteSecrets, req.Secrets)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deletes = append(f.deletes, req.ID)
	for name, s := range f.byName {
		if s.ID == req.ID {
			delete(f.byName, name)
		}
	}
	return nil
}

func (f *fakeSnapshotCSI) set(change func(f *fakeSnapshotCSI)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

// existing returns the IDs of the snapshots the plugin holds, sorted.
func (f *fakeSnapshotCSI) existing() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for _, s := range f.byName {
		ids = append(ids, s.ID)
	}
	sort.Strings(ids)
	return ids
}

func (f *fakeSnapshotCSI) counts() (creates, deletes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.creates), len(f.deletes)
}

// snapshotTestServer starts a server whose operator of snapshot schedules
// uses the fake plugin, with a volume "db" of a plugin "minnie" that can
// take snapshots (or not).
func snapshotTestServer(t *testing.T, acl bool, canSnapshot bool) (*certTest, *fakeSnapshotCSI, func()) {
	t.Helper()
	return snapshotTestServerWith(t, acl, canSnapshot, nil)
}

// snapshotTestServerWith is snapshotTestServer with a fake plugin set up
// before the server starts.
func snapshotTestServerWith(t *testing.T, acl bool, canSnapshot bool, setup func(*fakeSnapshotCSI)) (*certTest, *fakeSnapshotCSI, func()) {
	t.Helper()
	fake := newFakeSnapshotCSI()
	if setup != nil {
		setup(fake)
	}
	s, root, cleanup := secretTestServer(t, acl, func(c *Config) { c.snapshotCSI = fake })

	node := mock.Node()
	node.CSIControllerPlugins = map[string]*structs.CSIInfo{
		"minnie": {
			PluginID: "minnie",
			Healthy:  true,
			ControllerInfo: &structs.CSIControllerInfo{
				SupportsCreateDeleteSnapshot: canSnapshot,
			},
			RequiresControllerPlugin: true,
		},
	}
	must.NoError(t, s.State().UpsertNode(structs.MsgTypeTestSetup, 900, node))
	must.NoError(t, s.State().UpsertCSIVolume(901, []*structs.CSIVolume{{
		ID:             "db",
		Namespace:      structs.DefaultNamespace,
		AccessMode:     structs.CSIVolumeAccessModeSingleNodeWriter,
		AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem,
		PluginID:       "minnie",
		ExternalID:     "ext-db",
	}}))

	c := &certTest{t: t, s: s}
	if root != nil {
		c.token = root.SecretID
	}
	return c, fake, cleanup
}

// schedule returns a snapshot schedule and its decoded status.
func (c *certTest) schedule(name string) (*structs.CustomObject, *snapshotScheduleStatus) {
	c.t.Helper()
	obj := c.get("default", volumeSnapshotScheduleKind, name)
	if obj == nil {
		return nil, nil
	}
	st := &snapshotScheduleStatus{}
	must.NoError(c.t, structs.DecodeObjectPart(obj.Status, st))
	return obj, st
}

func (c *certTest) waitSchedule(name, what string, cond func(*structs.CustomObject, *snapshotScheduleStatus) bool) *snapshotScheduleStatus {
	c.t.Helper()
	var st *snapshotScheduleStatus
	must.Wait(c.t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			var obj *structs.CustomObject
			obj, st = c.schedule(name)
			return obj != nil && cond(obj, st)
		}),
		wait.Timeout(30*time.Second),
		wait.Gap(100*time.Millisecond),
	), must.Sprintf("snapshot schedule %s: %s", name, what))
	return st
}

// everySecond is a cron expression with a seconds field.
const everySecond = "* * * * * * *"

func TestSnapshotSchedule_TakesAndRetains(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := snapshotTestServer(t, false, true)
	defer cleanup()

	_, err := c.apply("default", volumeSnapshotScheduleKind, "db-often", map[string]any{
		"volume":   "db",
		"schedule": everySecond,
		"keep":     map[string]any{"last": 2},
	})
	must.NoError(t, err)

	st := c.waitSchedule("db-often", "four taken, two kept", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return st.Taken >= 4 && st.Kept == 2 && objectConditionTrue(obj.Status, structs.CertificateConditionReady)
	})
	must.Eq(t, "minnie", st.Plugin)
	must.Positive(t, st.Deleted)
	must.NotEq(t, "", st.NextRun)

	// The kept ones are the newest, the plugin holds no other snapshot of
	// the schedule (one taken between the reads may be left to delete).
	must.Eq(t, 2, len(st.Snapshots))
	must.True(t, st.Snapshots[0].Scheduled > st.Snapshots[1].Scheduled)
	for _, s := range st.Snapshots {
		must.Eq(t, "db", s.Volume)
		must.StrHasPrefix(t, "default-db-often-", s.Name)
		must.Eq(t, "snap-"+s.Name, s.ID)
		must.True(t, s.Ready)
	}
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return len(fake.existing()) <= 3
	}), wait.Timeout(10*time.Second), wait.Gap(100*time.Millisecond)))

	// Every snapshot the plugin made is one the schedule asked for by the
	// name of a scheduled time.
	fake.set(func(f *fakeSnapshotCSI) {
		for _, name := range f.creates {
			must.StrHasPrefix(t, "default-db-often-", name)
		}
	})
}

func TestSnapshotSchedule_Failures(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := snapshotTestServerWith(t, false, true, func(f *fakeSnapshotCSI) { f.retryBase = 200 * time.Millisecond })
	defer cleanup()

	fake.set(func(f *fakeSnapshotCSI) { f.createErr = errors.New("storage is full") })
	_, err := c.apply("default", volumeSnapshotScheduleKind, "db-fail", map[string]any{
		"volume":   "db",
		"schedule": everySecond,
		"keep":     map[string]any{"max_age": "5m"},
	})
	must.NoError(t, err)

	st := c.waitSchedule("db-fail", "failed", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return st.Failures > 0 && objectConditionTrue(obj.Status, snapshotConditionFailed) &&
			objectConditionTrue(obj.Status, structs.CertificateConditionDegraded)
	})
	must.NotEq(t, "", st.RetryAt)
	must.Eq(t, 0, st.Taken)
	obj, _ := c.schedule("db-fail")
	must.StrContains(t, snapshotFailure(obj.Status), "storage is full")

	fake.set(func(f *fakeSnapshotCSI) { f.createErr = nil })
	c.waitSchedule("db-fail", "taken after the failure", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return st.Taken > 0 && st.Failures == 0 && !objectConditionTrue(obj.Status, snapshotConditionFailed) &&
			objectConditionTrue(obj.Status, structs.CertificateConditionReady)
	})
}

func TestSnapshotSchedule_NotReadyAndUnsupported(t *testing.T) {
	ci.Parallel(t)

	t.Run("unsupported plugin", func(t *testing.T) {
		c, fake, cleanup := snapshotTestServer(t, false, false)
		defer cleanup()
		_, err := c.apply("default", volumeSnapshotScheduleKind, "db", map[string]any{
			"volume": "db", "schedule": everySecond, "keep": map[string]any{"last": 1},
		})
		must.NoError(t, err)
		c.waitSchedule("db", "unsupported", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
			return objectConditionTrue(obj.Status, snapshotConditionUnsupported) &&
				!objectConditionTrue(obj.Status, structs.CertificateConditionReady)
		})
		creates, _ := fake.counts()
		must.Zero(t, creates)
	})

	t.Run("missing volume and suspended", func(t *testing.T) {
		c, fake, cleanup := snapshotTestServer(t, false, true)
		defer cleanup()
		_, err := c.apply("default", volumeSnapshotScheduleKind, "gone", map[string]any{
			"volume": "nope", "schedule": everySecond, "keep": map[string]any{"last": 1},
		})
		must.NoError(t, err)
		c.waitSchedule("gone", "volume not found", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
			for _, cond := range operatorConditions(obj.Status) {
				if cond.Type == structs.CertificateConditionReady {
					return cond.Status == "False" && cond.Reason == "VolumeNotFound"
				}
			}
			return false
		})

		_, err = c.apply("default", volumeSnapshotScheduleKind, "paused", map[string]any{
			"volume": "db", "schedule": everySecond, "keep": map[string]any{"last": 1}, "suspend": true,
		})
		must.NoError(t, err)
		c.waitSchedule("paused", "suspended", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
			return objectConditionTrue(obj.Status, snapshotConditionSuspended) && st.NextRun != ""
		})
		time.Sleep(2 * time.Second)
		creates, _ := fake.counts()
		must.Zero(t, creates)
	})

	t.Run("unfinished snapshot", func(t *testing.T) {
		c, fake, cleanup := snapshotTestServerWith(t, false, true, func(f *fakeSnapshotCSI) { f.readyPoll = 100 * time.Millisecond })
		defer cleanup()

		fake.set(func(f *fakeSnapshotCSI) { f.notReady = true })
		_, err := c.apply("default", volumeSnapshotScheduleKind, "slow", map[string]any{
			"volume": "db", "schedule": "0 0 0 1 1 * 2099", "keep": map[string]any{"last": 1},
		})
		must.NoError(t, err)
		// The schedule's first time is far away: take one by hand through
		// the status of a past run, as a leader that took over would see.
		obj, _ := c.schedule("slow")
		must.NotNil(t, obj)

		st := &snapshotScheduleStatus{Snapshots: []*scheduledSnapshot{{
			ID: "snap-slow-1", Name: "slow-1", Volume: "db", Plugin: "minnie",
			Scheduled: time.Now().UTC().Format(time.RFC3339),
		}}}
		fake.set(func(f *fakeSnapshotCSI) { f.byName["slow-1"] = &structs.CSISnapshot{ID: "snap-slow-1"} })
		writeTestScheduleStatus(t, c.s, obj, st)

		time.Sleep(time.Second)
		_, st = c.schedule("slow")
		must.Eq(t, 1, len(st.Snapshots))
		must.False(t, st.Snapshots[0].Ready)

		fake.set(func(f *fakeSnapshotCSI) { f.notReady = false })
		c.waitSchedule("slow", "finished", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
			return len(st.Snapshots) == 1 && st.Snapshots[0].Ready && st.Snapshots[0].SizeBytes == 0
		})
	})
}

// writeTestScheduleStatus writes the status of a schedule as the servers
// do.
func writeTestScheduleStatus(t *testing.T, s *Server, obj *structs.CustomObject, st *snapshotScheduleStatus) {
	t.Helper()
	for range 20 {
		cur, err := s.State().CustomObjectByName(nil, obj.Group, obj.Kind, obj.Namespace, obj.Name)
		must.NoError(t, err)
		encoded, err := structs.EncodeObjectPart(st)
		must.NoError(t, err)
		changed := cur.Copy()
		changed.Status = encoded
		if _, err = s.writeOperatorObject(changed); err == nil {
			return
		}
		must.True(t, structs.IsObjectConflictError(err), must.Sprintf("%v", err))
	}
	t.Fatal("status of the schedule could not be written")
}

func TestSnapshotSchedule_Deletion(t *testing.T) {
	ci.Parallel(t)
	// A failed deletion is tried again after the wait of the operator.
	c, fake, cleanup := snapshotTestServerWith(t, false, true, func(f *fakeSnapshotCSI) { f.retryBase = 500 * time.Millisecond })
	defer cleanup()

	for _, name := range []string{"keep-them", "drop-them"} {
		_, err := c.apply("default", volumeSnapshotScheduleKind, name, map[string]any{
			"volume":           "db",
			"schedule":         everySecond,
			"keep":             map[string]any{"last": 3},
			"delete_snapshots": name == "drop-them",
		})
		must.NoError(t, err)
	}
	var dropped []string
	for _, name := range []string{"keep-them", "drop-them"} {
		st := c.waitSchedule(name, "two taken", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
			return st.Kept >= 2 && slices.Contains(obj.Finalizers, structs.OperatorsFinalizer)
		})
		if name == "drop-them" {
			for _, s := range st.Snapshots {
				dropped = append(dropped, s.ID)
			}
		}
	}

	// A snapshot that can't be deleted holds the object.
	fake.set(func(f *fakeSnapshotCSI) { f.deleteErr = errors.New("snapshot in use") })
	must.NoError(t, c.delete("default", volumeSnapshotScheduleKind, "drop-them"))
	c.waitSchedule("drop-them", "held by a snapshot", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return obj.Deleting() && len(st.Snapshots) > 0 && st.Snapshots[0].DeleteError != ""
	})

	fake.set(func(f *fakeSnapshotCSI) { f.deleteErr = nil })
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return c.get("default", volumeSnapshotScheduleKind, "drop-them") == nil
	}), wait.Timeout(20*time.Second), wait.Gap(100*time.Millisecond)))
	held := fake.existing()
	for _, id := range dropped {
		must.SliceNotContains(t, held, id)
	}

	// Without delete_snapshots the snapshots stay. The schedule is brought
	// to rest first: it takes a snapshot every second and deletes the
	// fourth, so a list read while it runs may name a snapshot the plugin
	// has deleted already. Suspended, with nothing left to delete, the
	// list is what the plugin holds of it.
	_, err := c.apply("default", volumeSnapshotScheduleKind, "keep-them", map[string]any{
		"volume": "db", "schedule": everySecond, "keep": map[string]any{"last": 3}, "suspend": true,
	})
	must.NoError(t, err)
	st := c.waitSchedule("keep-them", "at rest", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return objectConditionTrue(obj.Status, snapshotConditionSuspended) && len(st.Snapshots) > 0 && len(st.Snapshots) <= 3 &&
			!slices.ContainsFunc(st.Snapshots, func(s *scheduledSnapshot) bool { return !s.Ready || s.DeleteError != "" })
	})
	kept := st.Snapshots
	must.NoError(t, c.delete("default", volumeSnapshotScheduleKind, "keep-them"))
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return c.get("default", volumeSnapshotScheduleKind, "keep-them") == nil
	}), wait.Timeout(20*time.Second), wait.Gap(100*time.Millisecond)))
	held = fake.existing()
	for _, s := range kept {
		must.SliceContains(t, held, s.ID)
	}
}

func TestSnapshotSchedule_Secrets(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := snapshotTestServer(t, false, true)
	defer cleanup()

	var resp structs.SecretWriteResponse
	must.NoError(t, c.s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("csi/minnie", "", structs.SecretItems{"token": "s3cr3t"}), &resp))

	_, err := c.apply("default", volumeSnapshotScheduleKind, "db", map[string]any{
		"volume": "db", "schedule": everySecond, "keep": map[string]any{"last": 1}, "secret_path": "csi/minnie",
	})
	must.NoError(t, err)
	c.waitSchedule("db", "taken", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool { return st.Taken > 0 })
	fake.set(func(f *fakeSnapshotCSI) {
		must.Eq(t, structs.CSISecrets{"token": "s3cr3t"}, f.secrets[0])
	})

	// The secret is in no part of the object.
	obj := c.get("default", volumeSnapshotScheduleKind, "db")
	must.StrNotContains(t, fmt.Sprintf("%v %v", obj.Spec, obj.Status), "s3cr3t")

	// A secret that can't be read keeps the schedule from taking snapshots.
	_, err = c.apply("default", volumeSnapshotScheduleKind, "nosecret", map[string]any{
		"volume": "db", "schedule": everySecond, "keep": map[string]any{"last": 1}, "secret_path": "csi/missing",
	})
	must.NoError(t, err)
	c.waitSchedule("nosecret", "secret unavailable", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		for _, cond := range operatorConditions(obj.Status) {
			if cond.Type == structs.CertificateConditionReady {
				return cond.Status == "False" && cond.Reason == "SecretUnavailable"
			}
		}
		return false
	})
}

func TestSnapshotSchedule_WriteChecks(t *testing.T) {
	ci.Parallel(t)
	c, _, cleanup := snapshotTestServer(t, true, true)
	defer cleanup()

	base := func(change func(map[string]any)) map[string]any {
		spec := map[string]any{"volume": "db", "schedule": "0 3 * * *", "keep": map[string]any{"last": 7}}
		if change != nil {
			change(spec)
		}
		return spec
	}
	for name, tc := range map[string]struct {
		spec map[string]any
		want string
	}{
		"bad cron": {base(func(m map[string]any) { m["schedule"] = "every night" }), "not a cron expression"},
		"ended":    {base(func(m map[string]any) { m["schedule"] = "0 0 3 1 1 * 2025" }), "no time in the future"},
		"too many kept": {base(func(m map[string]any) {
			m["schedule"] = "*/5 * * * *"
			m["keep"] = map[string]any{"max_age": "168h"}
		}), "more than 500 snapshots"},
		"bad zone":      {base(func(m map[string]any) { m["time_zone"] = "Mars/Olympus" }), "time_zone"},
		"no keep":       {base(func(m map[string]any) { m["keep"] = map[string]any{} }), "keep needs"},
		"bad max_age":   {base(func(m map[string]any) { m["keep"] = map[string]any{"max_age": "30 days"} }), "max_age"},
		"bad prefix":    {base(func(m map[string]any) { m["name_prefix"] = "-x" }), "name_prefix"},
		"unknown field": {base(func(m map[string]any) { m["quiesce"] = true }), "quiesce"},
	} {
		_, err := c.apply("default", volumeSnapshotScheduleKind, "bad", tc.spec)
		must.ErrorContains(t, err, tc.want, must.Sprintf("%s", name))
	}

	_, err := c.apply("default", volumeSnapshotScheduleKind, "nightly", base(nil))
	must.NoError(t, err)
	_, err = c.apply("default", volumeSnapshotScheduleKind, "nightly", base(func(m map[string]any) { m["volume"] = "other" }))
	must.ErrorContains(t, err, "volume can't be changed")
	must.Eq(t, 403, errCode(t, c.status("default", volumeSnapshotScheduleKind, "nightly", map[string]any{"kept": 1})))

	// The writer needs the rights of the snapshot API, and to read the
	// secret of the CSI secrets.
	objects := mock.CreatePolicyAndToken(t, c.s.State(), 1001, "objects",
		`namespace "default" { object "norca.io/VolumeSnapshotSchedule" { capabilities = ["read", "write"] } }`)
	volumes := mock.CreatePolicyAndToken(t, c.s.State(), 1003, "volumes", `namespace "default" {
  object "norca.io/VolumeSnapshotSchedule" { capabilities = ["read", "write"] }
  capabilities = ["csi-write-volume"]
}
plugin { policy = "read" }`)
	both := mock.CreatePolicyAndToken(t, c.s.State(), 1005, "both", `namespace "default" {
  object "norca.io/VolumeSnapshotSchedule" { capabilities = ["read", "write"] }
  capabilities = ["csi-write-volume"]
  secret "csi/*" { capabilities = ["read-secret"] }
}
plugin { policy = "read" }`)

	noVolume := &certTest{t: t, s: c.s, token: objects.SecretID}
	_, err = noVolume.apply("default", volumeSnapshotScheduleKind, "w1", base(nil))
	must.Eq(t, 403, errCode(t, err))
	must.ErrorContains(t, err, "csi-write-volume")

	volumeOnly := &certTest{t: t, s: c.s, token: volumes.SecretID}
	_, err = volumeOnly.apply("default", volumeSnapshotScheduleKind, "w2", base(nil))
	must.NoError(t, err)
	_, err = volumeOnly.apply("default", volumeSnapshotScheduleKind, "w3", base(func(m map[string]any) { m["secret_path"] = "csi/minnie" }))
	must.Eq(t, 403, errCode(t, err))
	must.ErrorContains(t, err, "right to read its secret")

	writer := &certTest{t: t, s: c.s, token: both.SecretID}
	_, err = writer.apply("default", volumeSnapshotScheduleKind, "w3", base(func(m map[string]any) { m["secret_path"] = "csi/minnie" }))
	must.NoError(t, err)
}

func TestSnapshotScheduleTimes(t *testing.T) {
	ci.Parallel(t)

	expr, err := parseCron("0 3 * * *")
	must.NoError(t, err)
	berlin, err := time.LoadLocation("Europe/Berlin")
	must.NoError(t, err)

	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

	// Three nights were missed: only the last one is due.
	due, next := scheduleTimes(expr, time.UTC, base, now)
	must.Eq(t, time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC), due.UTC())
	must.Eq(t, time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC), next.UTC())

	// The time of the schedule is in its time zone.
	due, _ = scheduleTimes(expr, berlin, base, now)
	must.Eq(t, time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC), due.UTC())

	// Nothing due yet.
	due, next = scheduleTimes(expr, time.UTC, now, now.Add(time.Hour))
	must.True(t, due.IsZero())
	must.Eq(t, time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC), next.UTC())

	// A schedule idle for years searches the last year only.
	due, _ = scheduleTimes(expr, time.UTC, base.AddDate(-10, 0, 0), now)
	must.Eq(t, time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC), due.UTC())

	// The name of a time is the same for every leader.
	obj := &structs.CustomObject{ID: "0a1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9", Namespace: "prod", Name: "db-nightly"}
	must.Eq(t, "prod-db-nightly-0a1b2c3d-20261113-030000", snapshotName(obj, &snapshotScheduleSpec{}, due))
	must.Eq(t, "db-0a1b2c3d-20261113-030000", snapshotName(obj, &snapshotScheduleSpec{NamePrefix: "db"}, due.In(berlin)))
}

// A snapshot kept by another rule than max_age, past its max_age, must not
// make the pass ask for a run in the past (review finding SNP-2).
func TestSnapshotRetain_NoPastWish(t *testing.T) {
	ci.Parallel(t)

	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	st := &snapshotScheduleStatus{}
	for i := 1; i <= 3; i++ {
		st.Snapshots = append(st.Snapshots, &scheduledSnapshot{
			ID: fmt.Sprintf("snap-%d", i), Name: fmt.Sprintf("s-%d", i), Volume: "db", Plugin: "minnie", Ready: true,
			Scheduled: now.Add(-time.Duration(i) * 48 * time.Hour).Format(time.RFC3339),
		})
	}
	// One younger than max_age: the pass wants to run when it ages out.
	st.Snapshots = append(st.Snapshots, &scheduledSnapshot{
		ID: "snap-0", Name: "s-0", Volume: "db", Plugin: "minnie", Ready: true,
		Scheduled: now.Add(-time.Hour).Format(time.RFC3339),
	})

	o := &snapshotOperator{}
	p := &snapshotPass{now: now}
	o.retain(p, &structs.CustomObject{Namespace: "default", Name: "db"}, st, 24*time.Hour, 10, nil)
	must.Eq(t, 4, len(st.Snapshots))
	must.Eq(t, now.Add(23*time.Hour), p.wish, must.Sprintf("wish %s, now %s", p.wish, now))
}

// The loop of the operators does not spin on a wish in the past, whichever
// operator returns it (review finding SNP-2).
func TestSnapshotOperatorWake(t *testing.T) {
	ci.Parallel(t)

	started := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	next := started.Add(operatorsInterval)

	must.Eq(t, next, operatorWake(next, time.Time{}, started))
	must.Eq(t, started.Add(time.Second), operatorWake(next, started.Add(time.Second), started))
	must.Eq(t, next, operatorWake(next, next.Add(time.Hour), started))
	// A wish in the past, or at the start of the pass: not at once.
	must.Eq(t, started.Add(operatorsRetry), operatorWake(next, started.Add(-time.Hour), started))
	must.Eq(t, started.Add(operatorsRetry), operatorWake(next, started, started))
	// An earlier wish of another operator stands.
	must.Eq(t, started.Add(time.Second), operatorWake(started.Add(time.Second), started.Add(-time.Hour), started))
}

// A schedule with a year field ends: its last time is still taken, and the
// retention goes on after it (review finding SNP-9).
func TestSnapshotSchedule_FiniteSchedule(t *testing.T) {
	ci.Parallel(t)

	spec := &snapshotScheduleSpec{Schedule: "0 0 3 1 1 * 2025", Keep: snapshotKeep{Last: 1}}
	expr, loc, _, err := spec.parse()
	must.NoError(t, err)

	last := time.Date(2025, 1, 1, 3, 0, 0, 0, time.UTC)
	due, next := scheduleTimes(expr, loc, last.Add(-time.Hour), last.Add(time.Minute))
	must.Eq(t, last, due.UTC())
	must.True(t, next.IsZero())

	// Taken: nothing is due any more.
	due, next = scheduleTimes(expr, loc, last, last.Add(time.Minute))
	must.True(t, due.IsZero())
	must.True(t, next.IsZero())
}

// The walk to the latest time that has come does not grow with the time
// the schedule was not run (review finding SNP-12).
func TestSnapshotScheduleTimes_LongIdle(t *testing.T) {
	ci.Parallel(t)

	expr, err := parseCron(everySecond)
	must.NoError(t, err)
	now := time.Date(2026, 11, 17, 12, 0, 0, 500, time.UTC)

	type times struct{ due, next time.Time }
	done := make(chan times, 1)
	go func() {
		due, next := scheduleTimes(expr, time.UTC, now.AddDate(0, -11, 0), now)
		done <- times{due, next}
	}()
	select {
	case got := <-done:
		must.Eq(t, now.Truncate(time.Second), got.due.UTC())
		must.Eq(t, now.Truncate(time.Second).Add(time.Second), got.next.UTC())
	case <-time.After(3 * time.Second):
		t.Fatal("scheduleTimes walks every time since the last run")
	}

	// The times are those of the plain walk from the base, also across
	// the changes of daylight saving time.
	berlin, err := time.LoadLocation("Europe/Berlin")
	must.NoError(t, err)
	for _, schedule := range []string{"0 3 * * *", "30 2 * * *", "*/7 * * * *", "0 0 1 * *", "15 10 * * 1", "0 0 3 1 1 * 2026"} {
		expr, err := parseCron(schedule)
		must.NoError(t, err)
		for _, loc := range []*time.Location{time.UTC, berlin} {
			for _, base := range []time.Time{
				time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
				time.Date(2026, 10, 20, 7, 3, 0, 0, time.UTC),
			} {
				for _, idle := range []time.Duration{time.Second, 50 * time.Minute, 9 * 24 * time.Hour, 40 * 24 * time.Hour} {
					now := base.Add(idle)
					var wantDue time.Time
					wantNext := expr.Next(base.In(loc))
					for !wantNext.IsZero() && !wantNext.After(now) {
						wantDue, wantNext = wantNext, expr.Next(wantNext)
					}
					due, next := scheduleTimes(expr, loc, base, now)
					must.True(t, wantDue.Equal(due) && wantNext.Equal(next),
						must.Sprintf("%s in %s from %s for %s: due %s next %s, the walk has %s and %s",
							schedule, loc, base, idle, due, next, wantDue, wantNext))
				}
			}
		}
	}
}

// The names of the snapshots of two schedules never meet (review finding
// SNP-7).
func TestSnapshotName_UniquePerSchedule(t *testing.T) {
	ci.Parallel(t)

	at := time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC)
	a := &structs.CustomObject{ID: "11111111-aaaa-bbbb-cccc-000000000001", Namespace: "prod", Name: "db-nightly"}
	b := &structs.CustomObject{ID: "22222222-aaaa-bbbb-cccc-000000000002", Namespace: "prod-db", Name: "nightly"}
	must.NotEq(t, snapshotName(a, &snapshotScheduleSpec{}, at), snapshotName(b, &snapshotScheduleSpec{}, at))

	// The same name_prefix on two schedules.
	shared := &snapshotScheduleSpec{NamePrefix: "db"}
	must.NotEq(t, snapshotName(a, shared, at), snapshotName(b, shared, at))

	// A long namespace and name: the name stays within what CSI allows,
	// and still tells the schedules apart.
	long := &structs.CustomObject{ID: a.ID, Namespace: strings.Repeat("n", 128), Name: strings.Repeat("s", 128)}
	other := &structs.CustomObject{ID: b.ID, Namespace: long.Namespace, Name: long.Name}
	name := snapshotName(long, &snapshotScheduleSpec{}, at)
	must.Eq(t, 89, len(name))
	must.StrHasSuffix(t, "-11111111-20261113-030000", name)
	must.NotEq(t, name, snapshotName(other, &snapshotScheduleSpec{}, at))
}

// farSchedule is a cron expression whose first time is far away: the tests
// that write the status of a schedule by hand use it.
const farSchedule = "0 0 0 1 1 * 2099"

// listedSnapshots returns n finished snapshots of the volume "db" for the
// status of a schedule, the newest first, an hour apart, and puts them into
// the fake plugin.
func listedSnapshots(fake *fakeSnapshotCSI, prefix string, n int) []*scheduledSnapshot {
	var out []*scheduledSnapshot
	now := time.Now().UTC()
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("%s-%d", prefix, i)
		out = append(out, &scheduledSnapshot{
			ID: "snap-" + name, Name: name, Volume: "db", Plugin: "minnie", Ready: true,
			Scheduled: now.Add(-time.Duration(i) * time.Hour).Format(time.RFC3339),
		})
		fake.set(func(f *fakeSnapshotCSI) { f.byName[name] = &structs.CSISnapshot{ID: "snap-" + name, IsReady: true} })
	}
	return out
}

// While the secret of a deleted schedule can't be read, none of its
// snapshots is sent to the plugin (review finding SNP-10).
func TestSnapshotSchedule_DeletionHeldBySecret(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := snapshotTestServer(t, false, true)
	defer cleanup()

	_, err := c.apply("default", volumeSnapshotScheduleKind, "held", map[string]any{
		"volume": "db", "schedule": farSchedule, "keep": map[string]any{"last": 5},
		"secret_path": "csi/later", "delete_snapshots": true,
	})
	must.NoError(t, err)
	c.waitSchedule("held", "finalizer", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return slices.Contains(obj.Finalizers, structs.OperatorsFinalizer)
	})
	obj, _ := c.schedule("held")
	writeTestScheduleStatus(t, c.s, obj, &snapshotScheduleStatus{Snapshots: listedSnapshots(fake, "held", 3)})

	must.NoError(t, c.delete("default", volumeSnapshotScheduleKind, "held"))
	st := c.waitSchedule("held", "held by the secret", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return obj.Deleting() && len(st.Snapshots) > 0 && st.Snapshots[0].DeleteError != ""
	})
	must.Eq(t, 3, len(st.Snapshots))
	for _, s := range st.Snapshots {
		must.StrContains(t, s.DeleteError, "csi/later")
	}
	fake.set(func(f *fakeSnapshotCSI) { must.Eq(t, 0, len(f.deleteSecrets)) })
	must.Eq(t, 3, len(fake.existing()))

	// With the secret the snapshots go, each with it.
	var resp structs.SecretWriteResponse
	must.NoError(t, c.s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("csi/later", "", structs.SecretItems{"token": "s3cr3t"}), &resp))
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return c.get("default", volumeSnapshotScheduleKind, "held") == nil
	}), wait.Timeout(40*time.Second), wait.Gap(100*time.Millisecond)))
	must.Eq(t, 0, len(fake.existing()))
	fake.set(func(f *fakeSnapshotCSI) {
		must.Eq(t, 3, len(f.deleteSecrets))
		for _, secrets := range f.deleteSecrets {
			must.Eq(t, structs.CSISecrets{"token": "s3cr3t"}, secrets)
		}
	})
}

// A deletion gets the secrets of the volume, as the creation does, under
// those of secret_path (review finding SNP-3).
func TestSnapshotSchedule_DeleteWithVolumeSecrets(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := snapshotTestServer(t, false, true)
	defer cleanup()

	vol, err := c.s.State().CSIVolumeByID(nil, structs.DefaultNamespace, "db")
	must.NoError(t, err)
	vol = vol.Copy()
	vol.Secrets = structs.CSISecrets{"volume-key": "of the volume", "token": "of the volume"}
	must.NoError(t, c.s.State().UpsertCSIVolume(902, []*structs.CSIVolume{vol}))

	var resp structs.SecretWriteResponse
	must.NoError(t, c.s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("csi/minnie", "", structs.SecretItems{"token": "s3cr3t"}), &resp))

	_, err = c.apply("default", volumeSnapshotScheduleKind, "db", map[string]any{
		"volume": "db", "schedule": farSchedule, "keep": map[string]any{"last": 1}, "secret_path": "csi/minnie",
	})
	must.NoError(t, err)
	c.waitSchedule("db", "finalizer", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return slices.Contains(obj.Finalizers, structs.OperatorsFinalizer)
	})
	obj, _ := c.schedule("db")
	writeTestScheduleStatus(t, c.s, obj, &snapshotScheduleStatus{Snapshots: listedSnapshots(fake, "db", 2)})

	c.waitSchedule("db", "the older one deleted", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return st.Deleted == 1 && len(st.Snapshots) == 1
	})
	fake.set(func(f *fakeSnapshotCSI) {
		must.Eq(t, []structs.CSISecrets{{"volume-key": "of the volume", "token": "s3cr3t"}}, f.deleteSecrets)
	})
}

// An event of an object does not stand in the way of the status write
// that follows it (the mechanism review finding SNP-5 supposed).
func TestSnapshotSchedule_StatusWriteAfterEvent(t *testing.T) {
	ci.Parallel(t)
	c, _, cleanup := snapshotTestServer(t, false, true)
	defer cleanup()

	_, err := c.apply("default", volumeSnapshotScheduleKind, "db", map[string]any{
		"volume": "db", "schedule": farSchedule, "keep": map[string]any{"last": 1},
	})
	must.NoError(t, err)
	c.waitSchedule("db", "finalizer", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return slices.Contains(obj.Finalizers, structs.OperatorsFinalizer)
	})
	written := 0
	for i := 1; i <= 20; i++ {
		cur := c.get("default", volumeSnapshotScheduleKind, "db")
		c.s.operatorEvent(cur, structs.ObjectEventTypeWarning, "SnapshotDeleteFailed", "an event in the second of the write")
		changed := cur.Copy()
		changed.Status = structs.CopyObjectMap(cur.Status)
		changed.Status["taken"] = i
		_, err := c.s.writeOperatorObject(changed)
		if err != nil && structs.IsObjectConflictError(err) {
			continue
		}
		must.NoError(t, err, must.Sprintf("write %d", i))
		written++
	}
	must.Positive(t, written)
	log, err := c.s.State().ObjectEventLog(nil, structs.OperatorsGroup, volumeSnapshotScheduleKind, "default", "db")
	must.NoError(t, err)
	must.NotNil(t, log)
}

// A snapshot the plugin made is in the list of its schedule, also when the
// answer of the plugin was lost (review finding SNP-4).
func TestSnapshotSchedule_LostAnswer(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := snapshotTestServerWith(t, false, true, func(f *fakeSnapshotCSI) { f.lostAnswers = 1 })
	defer cleanup()

	_, err := c.apply("default", volumeSnapshotScheduleKind, "db-lost", map[string]any{
		"volume": "db", "schedule": everySecond, "keep": map[string]any{"last": 50},
	})
	must.NoError(t, err)
	c.waitSchedule("db-lost", "taken after the lost answer", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return st.Taken >= 3
	})

	// What the plugin holds was written into the status before it was
	// asked for.
	held := fake.names()
	_, st := c.schedule("db-lost")
	var listed []string
	for _, s := range st.Snapshots {
		listed = append(listed, s.Name)
	}
	must.GreaterEq(t, 3, len(held))
	for _, name := range held {
		must.SliceContains(t, listed, name)
	}
}

// The wait after a failed snapshot holds only until the next scheduled
// time: that one is tried at its time (review finding SNP-11). The request
// is the one that failed, by its name.
func TestSnapshotSchedule_RetryAtTheNextTime(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := snapshotTestServerWith(t, false, true, func(f *fakeSnapshotCSI) {
		f.retryBase = time.Hour
		f.createErr = errors.New("storage is full")
	})
	defer cleanup()

	_, err := c.apply("default", volumeSnapshotScheduleKind, "db-retry", map[string]any{
		"volume": "db", "schedule": everySecond, "keep": map[string]any{"last": 5},
	})
	must.NoError(t, err)
	c.waitSchedule("db-retry", "tried again at the later times", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return st.Failures >= 3 && st.RetryAt != ""
	})
	fake.set(func(f *fakeSnapshotCSI) {
		for _, name := range f.creates {
			must.Eq(t, f.creates[0], name)
		}
		f.createErr = nil
	})
	st := c.waitSchedule("db-retry", "taken", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return st.Taken >= 1 && st.Failures == 0
	})
	must.Eq(t, "", st.RetryAt)
}

// A snapshot the plugin was still making: when asking for it fails the
// schedule says so, and when the plugin made it again the list has the new
// ID (review finding SNP-13).
func TestSnapshotSchedule_UnfinishedSnapshot(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := snapshotTestServerWith(t, false, true, func(f *fakeSnapshotCSI) { f.readyPoll = 100 * time.Millisecond })
	defer cleanup()

	_, err := c.apply("default", volumeSnapshotScheduleKind, "slow", map[string]any{
		"volume": "db", "schedule": farSchedule, "keep": map[string]any{"last": 1},
	})
	must.NoError(t, err)
	c.waitSchedule("slow", "finalizer", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return slices.Contains(obj.Finalizers, structs.OperatorsFinalizer)
	})

	fake.set(func(f *fakeSnapshotCSI) {
		f.createErr = errors.New("snapshot exists with other parameters")
		f.byName["slow-1"] = &structs.CSISnapshot{ID: "snap-new", SizeBytes: 7}
	})
	obj, _ := c.schedule("slow")
	writeTestScheduleStatus(t, c.s, obj, &snapshotScheduleStatus{Snapshots: []*scheduledSnapshot{{
		ID: "snap-old", Name: "slow-1", Volume: "db", Plugin: "minnie",
		Scheduled: time.Now().UTC().Format(time.RFC3339),
	}}})

	c.waitSchedule("slow", "the failure shown", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return objectConditionTrue(obj.Status, snapshotConditionFailed) &&
			objectConditionTrue(obj.Status, structs.CertificateConditionDegraded)
	})
	obj, st := c.schedule("slow")
	must.StrContains(t, snapshotFailure(obj.Status), "slow-1")
	must.StrContains(t, snapshotFailure(obj.Status), "other parameters")
	must.Eq(t, "snap-old", st.Snapshots[0].ID)
	must.Eq(t, 0, st.Failures)

	fake.set(func(f *fakeSnapshotCSI) { f.createErr = nil })
	c.waitSchedule("slow", "finished under the new ID", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return len(st.Snapshots) == 1 && st.Snapshots[0].ID == "snap-new" && st.Snapshots[0].Ready &&
			!objectConditionTrue(obj.Status, snapshotConditionFailed)
	})
}

// deleteFailedEvents counts the SnapshotDeleteFailed events of a schedule.
func (c *certTest) deleteFailedEvents(name string) int {
	c.t.Helper()
	log, err := c.s.State().ObjectEventLog(nil, structs.OperatorsGroup, volumeSnapshotScheduleKind, "default", name)
	must.NoError(c.t, err)
	n := 0
	if log != nil {
		for _, e := range log.Events {
			if e.Reason == "SnapshotDeleteFailed" {
				n += e.Count
			}
		}
	}
	return n
}

// A deletion that fails is tried again with a growing wait, and is one
// event, not one per attempt (review finding SNP-5).
func TestSnapshotSchedule_DeleteBackoff(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := snapshotTestServerWith(t, false, true, func(f *fakeSnapshotCSI) {
		f.retryBase = time.Second
		f.deleteErr = errors.New("snapshot in use")
	})
	defer cleanup()

	_, err := c.apply("default", volumeSnapshotScheduleKind, "db", map[string]any{
		"volume": "db", "schedule": farSchedule, "keep": map[string]any{"last": 1},
	})
	must.NoError(t, err)
	c.waitSchedule("db", "finalizer", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return slices.Contains(obj.Finalizers, structs.OperatorsFinalizer)
	})
	obj, _ := c.schedule("db")
	writeTestScheduleStatus(t, c.s, obj, &snapshotScheduleStatus{Snapshots: listedSnapshots(fake, "db", 3)})

	// Three rounds of the two deletions.
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		n := 0
		fake.set(func(f *fakeSnapshotCSI) { n = len(f.deleteSecrets) })
		return n >= 6
	}), wait.Timeout(40*time.Second), wait.Gap(100*time.Millisecond)))
	st := c.waitSchedule("db", "the failures in the status", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return len(st.Snapshots) == 3 && st.Snapshots[1].DeleteError != "" && st.Snapshots[2].DeleteError != ""
	})
	must.Eq(t, "", st.Snapshots[0].DeleteError)
	must.Eq(t, 2, c.deleteFailedEvents("db"))
	must.GreaterEq(t, 3, st.DeleteFailures)
	must.NotEq(t, "", st.DeleteRetryAt)

	fake.set(func(f *fakeSnapshotCSI) { f.deleteErr = nil })
	st = c.waitSchedule("db", "deleted", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return st.Deleted == 2 && len(st.Snapshots) == 1
	})
	must.Eq(t, 0, st.DeleteFailures)
	must.Eq(t, "", st.DeleteRetryAt)
	must.Eq(t, 2, c.deleteFailedEvents("db"))
}

// While the secret of a schedule can't be read, its retention sends no
// deletion to the plugin (review finding SNP-5).
func TestSnapshotSchedule_RetentionWaitsForSecret(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := snapshotTestServer(t, false, true)
	defer cleanup()

	_, err := c.apply("default", volumeSnapshotScheduleKind, "db", map[string]any{
		"volume": "db", "schedule": farSchedule, "keep": map[string]any{"last": 1}, "secret_path": "csi/later",
	})
	must.NoError(t, err)
	c.waitSchedule("db", "finalizer", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return slices.Contains(obj.Finalizers, structs.OperatorsFinalizer)
	})
	obj, _ := c.schedule("db")
	writeTestScheduleStatus(t, c.s, obj, &snapshotScheduleStatus{Snapshots: listedSnapshots(fake, "db", 3)})

	st := c.waitSchedule("db", "secret unavailable", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		for _, cond := range operatorConditions(obj.Status) {
			if cond.Type == structs.CertificateConditionReady {
				return cond.Status == "False" && cond.Reason == "SecretUnavailable"
			}
		}
		return false
	})
	must.Eq(t, 3, len(st.Snapshots))
	must.Eq(t, 0, st.Deleted)
	fake.set(func(f *fakeSnapshotCSI) { must.Eq(t, 0, len(f.deleteSecrets)) })

	// With the secret the retention goes on.
	var resp structs.SecretWriteResponse
	must.NoError(t, c.s.RPC(structs.SecretUpsertRPCMethod, secretWriteReq("csi/later", "", structs.SecretItems{"token": "s3cr3t"}), &resp))
	c.waitSchedule("db", "deleted with the secret", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return st.Deleted == 2 && len(st.Snapshots) == 1
	})
	fake.set(func(f *fakeSnapshotCSI) {
		must.Eq(t, []structs.CSISecrets{{"token": "s3cr3t"}, {"token": "s3cr3t"}}, f.deleteSecrets)
	})
}

// A status that has no room for another snapshot: the schedule takes none,
// and says so, instead of making snapshots it can't write down (review
// finding SNP-1).
func TestSnapshotSchedule_StatusFull(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := snapshotTestServer(t, false, true)
	defer cleanup()

	_, err := c.apply("default", volumeSnapshotScheduleKind, "db", map[string]any{
		"volume": "db", "schedule": everySecond, "keep": map[string]any{"last": 500}, "suspend": true,
	})
	must.NoError(t, err)
	c.waitSchedule("db", "suspended", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return objectConditionTrue(obj.Status, snapshotConditionSuspended)
	})

	// 330 snapshots with long IDs: over 200 KiB of the 256 KiB an object
	// may have.
	var listed []*scheduledSnapshot
	now := time.Now().UTC()
	for i := 1; i <= 330; i++ {
		listed = append(listed, &scheduledSnapshot{
			ID: fmt.Sprintf("snap-%04d-%s", i, strings.Repeat("x", 480)), Name: fmt.Sprintf("old-%04d", i),
			Volume: "db", Plugin: "minnie", Ready: true,
			Scheduled: now.Add(-time.Duration(i) * time.Hour).Format(time.RFC3339),
		})
	}
	obj, _ := c.schedule("db")
	writeTestScheduleStatus(t, c.s, obj, &snapshotScheduleStatus{Snapshots: listed})

	_, err = c.apply("default", volumeSnapshotScheduleKind, "db", map[string]any{
		"volume": "db", "schedule": everySecond, "keep": map[string]any{"last": 500},
	})
	must.NoError(t, err)
	c.waitSchedule("db", "status full", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		for _, cond := range operatorConditions(obj.Status) {
			if cond.Type == structs.CertificateConditionReady {
				return cond.Status == "False" && cond.Reason == "StatusFull"
			}
		}
		return false
	})
	time.Sleep(2 * time.Second)
	creates, _ := fake.counts()
	must.Zero(t, creates)
	_, st := c.schedule("db")
	must.Eq(t, 330, len(st.Snapshots))
	must.Eq(t, 0, st.Taken)

	// With room again (a shorter retention) the schedule goes on.
	_, err = c.apply("default", volumeSnapshotScheduleKind, "db", map[string]any{
		"volume": "db", "schedule": everySecond, "keep": map[string]any{"last": 5},
	})
	must.NoError(t, err)
	c.waitSchedule("db", "taking again", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return st.Taken > 0 && objectConditionTrue(obj.Status, structs.CertificateConditionReady)
	})
}

// What a status keeps of a failed deletion is bounded (review finding
// SNP-1).
func TestSnapshotStatus_Bounded(t *testing.T) {
	ci.Parallel(t)

	must.Eq(t, "short", snapshotErrorText(errors.New("short")))
	long := snapshotErrorText(errors.New(strings.Repeat("\u00e9", 1000)))
	must.LessEq(t, snapshotDeleteErrorMax, len(long))
	must.True(t, utf8.ValidString(long))

	// A list whose errors do not fit: the errors go, the snapshots stay.
	obj := &structs.CustomObject{Spec: map[string]any{"volume": "db"}}
	st := &snapshotScheduleStatus{}
	for i := 0; i < 900; i++ {
		st.Snapshots = append(st.Snapshots, &scheduledSnapshot{
			ID: fmt.Sprintf("snap-%04d", i), Name: fmt.Sprintf("s-%04d", i), Volume: "db", Plugin: "minnie", Ready: true,
			Scheduled: "2026-11-17T12:00:00Z", DeleteError: strings.Repeat("e", snapshotDeleteErrorMax),
		})
	}
	must.Greater(t, structs.MaxCustomObjectSize, snapshotStatusSize(obj, st))
	snapshotFitStatus(obj, st)
	must.Less(t, structs.MaxCustomObjectSize-16*1024, snapshotStatusSize(obj, st))
	must.Eq(t, 900, len(st.Snapshots))
	must.NotEq(t, "", st.Snapshots[0].DeleteError)
}

// A pass of the operator has a time budget: a plugin that answers slowly
// does not hold the loop of the built-in operators for all the schedules
// in one pass, and every schedule gets its turn (review finding SNP-6).
func TestSnapshotSchedule_PassBudget(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := snapshotTestServerWith(t, false, true, func(f *fakeSnapshotCSI) {
		f.passBudget = 100 * time.Millisecond
		f.createDelay = 300 * time.Millisecond
	})
	defer cleanup()

	names := []string{"slow-a", "slow-b", "slow-c"}
	for _, name := range names {
		_, err := c.apply("default", volumeSnapshotScheduleKind, name, map[string]any{
			"volume": "db", "schedule": everySecond, "keep": map[string]any{"last": 50},
		})
		must.NoError(t, err)
	}
	for _, name := range names {
		c.waitSchedule(name, "its turn, twice", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
			return st.Taken >= 2
		})
	}
	fake.set(func(f *fakeSnapshotCSI) {
		perPass := map[*state.StateSnapshot]int{}
		for _, pass := range f.createPasses {
			perPass[pass]++
			must.Eq(t, 1, perPass[pass], must.Sprint("slow calls of one pass"))
		}
	})
}

// Each built-in operator has a loop of its own: while the call of a CSI
// plugin holds the operator of snapshot schedules, the others go on. When
// the leadership is lost no pass starts any more, and the loops end once the
// pass in flight has (review finding SNP-6).
func TestOperators_OwnLoops(t *testing.T) {
	ci.Parallel(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	c, fake, cleanup := snapshotTestServerWith(t, true, true, func(f *fakeSnapshotCSI) {
		f.createBlock, f.createEntered = release, entered
	})
	defer cleanup()
	waitTenantKind(t, c.s)

	_, err := c.apply("default", volumeSnapshotScheduleKind, "stuck", map[string]any{
		"volume": "db", "schedule": everySecond, "keep": map[string]any{"last": 5},
	})
	must.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the snapshot operator did not call the plugin")
	}

	// The plugin does not answer: the pass of the snapshot operator stays
	// in its call. A tenant written now is still brought about.
	_, err = c.apply("", structs.TenantKind, "team-loop", map[string]any{
		"namespace": map[string]any{"description": "written while a plugin call hangs"},
	})
	must.NoError(t, err)
	waitTenant(c, "team-loop", "ready while the snapshot operator waits", tenantReady)
	creates, _ := fake.counts()
	must.Eq(t, 0, creates, must.Sprint("the plugin call is still held"))

	// Leadership is lost (the server shuts down): the loops of the idle
	// operators end, the pass in flight is waited for and no other one
	// starts.
	stopped := c.s.operators.stopped()
	must.NotNil(t, stopped)
	must.NoError(t, c.s.Shutdown())
	select {
	case <-stopped:
		t.Fatal("the loops ended while a pass was in flight")
	case <-time.After(time.Second):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		t.Fatal("the loops of the operators did not end")
	}
	creates, _ = fake.counts()
	time.Sleep(1500 * time.Millisecond)
	after, _ := fake.counts()
	must.Eq(t, creates, after, must.Sprint("a pass started after the loops ended"))
}

// A schedule that was written after the pass read it is not acted on, and
// the retention stops between two deletions when the schedule changes: no
// snapshot is deleted by a keep rule the schedule no longer has.
func TestSnapshotSchedule_StaleScheduleIsNotActedOn(t *testing.T) {
	ci.Parallel(t)
	c, fake, cleanup := snapshotTestServer(t, false, true)
	defer cleanup()

	// The plugin of the operator of the server refuses deletions: after
	// its first try that operator waits, and the list stays as it is.
	fake.set(func(f *fakeSnapshotCSI) { f.deleteErr = errors.New("not now") })
	spec := func(last int) map[string]any {
		return map[string]any{"volume": "db", "schedule": farSchedule, "keep": map[string]any{"last": last}}
	}
	_, err := c.apply("default", volumeSnapshotScheduleKind, "db", spec(5))
	must.NoError(t, err)
	c.waitSchedule("db", "finalizer", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return slices.Contains(obj.Finalizers, structs.OperatorsFinalizer)
	})
	obj, _ := c.schedule("db")
	writeTestScheduleStatus(t, c.s, obj, &snapshotScheduleStatus{Snapshots: listedSnapshots(fake, "db", 4)})
	_, err = c.apply("default", volumeSnapshotScheduleKind, "db", spec(1))
	must.NoError(t, err)
	c.waitSchedule("db", "the operator of the server waits", func(obj *structs.CustomObject, st *snapshotScheduleStatus) bool {
		return st.DeleteRetryAt != "" && len(st.Snapshots) == 4
	})

	// A second operator with a plugin of its own, driven by hand.
	plugin := newFakeSnapshotCSI()
	o := newSnapshotOperator(c.s, c.s.logger)
	o.csi = plugin
	pass := func() *snapshotPass {
		snap, err := c.s.State().Snapshot()
		must.NoError(t, err)
		return &snapshotPass{now: time.Now().UTC().Add(2 * time.Hour), store: c.s.State(), snap: snap, ws: memdb.NewWatchSet()}
	}

	// Read with keep.last 1; the user makes it 2 before the pass comes to
	// the schedule.
	read, _ := c.schedule("db")
	_, err = c.apply("default", volumeSnapshotScheduleKind, "db", spec(2))
	must.NoError(t, err)
	o.reconcileSchedule(pass(), read)
	plugin.set(func(f *fakeSnapshotCSI) {
		must.Eq(t, 0, len(f.deleteSecrets), must.Sprint("deleted by the keep rule that was changed"))
	})

	// With keep.last 2 two snapshots go. While the plugin deletes the
	// first, the user makes it 5: the second one stays.
	var once sync.Once
	plugin.set(func(f *fakeSnapshotCSI) {
		f.onDelete = func() {
			once.Do(func() {
				_, err := c.apply("default", volumeSnapshotScheduleKind, "db", spec(5))
				must.NoError(t, err)
			})
		}
	})
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		// The operator of the server may write the status once more
		// after the change: the pass needs the schedule as it is stored.
		cur, _ := c.schedule("db")
		o.reconcileSchedule(pass(), cur)
		calls := 0
		plugin.set(func(f *fakeSnapshotCSI) { calls = len(f.deleteSecrets) })
		return calls > 0
	}), wait.Timeout(20*time.Second), wait.Gap(200*time.Millisecond)))
	plugin.set(func(f *fakeSnapshotCSI) {
		must.Eq(t, 1, len(f.deleteSecrets), must.Sprint("the retention went on after the schedule changed"))
	})
}

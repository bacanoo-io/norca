// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// fakeSnapshotCSI is a controller plugin of a test: a snapshot of a name is
// created once, as CSI asks.
type fakeSnapshotCSI struct {
	mu        sync.Mutex
	byName    map[string]*structs.CSISnapshot
	creates   []string
	deletes   []string
	secrets   []structs.CSISecrets
	createErr error
	deleteErr error
	notReady  bool
}

func newFakeSnapshotCSI() *fakeSnapshotCSI {
	return &fakeSnapshotCSI{byName: map[string]*structs.CSISnapshot{}}
}

func (f *fakeSnapshotCSI) create(_ *state.StateSnapshot, _ string, req *structs.CSISnapshot) (*structs.CSISnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	out := *snap
	return &out, nil
}

func (f *fakeSnapshotCSI) delete(_ *state.StateSnapshot, req *structs.CSISnapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	fake := newFakeSnapshotCSI()
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
	c, fake, cleanup := snapshotTestServer(t, false, true)
	defer cleanup()

	oldBase := snapshotRetryBase
	snapshotRetryBase = 200 * time.Millisecond
	defer func() { snapshotRetryBase = oldBase }()

	fake.set(func(f *fakeSnapshotCSI) { f.createErr = errors.New("storage is full") })
	_, err := c.apply("default", volumeSnapshotScheduleKind, "db-fail", map[string]any{
		"volume":   "db",
		"schedule": everySecond,
		"keep":     map[string]any{"max_age": "1h"},
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
		c, fake, cleanup := snapshotTestServer(t, false, true)
		defer cleanup()
		oldPoll := snapshotReadyPoll
		snapshotReadyPoll = 100 * time.Millisecond
		defer func() { snapshotReadyPoll = oldPoll }()

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
	c, fake, cleanup := snapshotTestServer(t, false, true)
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

	// Without delete_snapshots the snapshots stay.
	_, st := c.schedule("keep-them")
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
		"bad cron":      {base(func(m map[string]any) { m["schedule"] = "every night" }), "not a cron expression"},
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
	obj := &structs.CustomObject{Namespace: "prod", Name: "db-nightly"}
	must.Eq(t, "prod-db-nightly-20261113-030000", snapshotName(obj, &snapshotScheduleSpec{}, due))
	must.Eq(t, "db-20261113-030000", snapshotName(obj, &snapshotScheduleSpec{NamePrefix: "db"}, due.In(berlin)))
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

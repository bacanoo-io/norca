// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package backup

import (
	"slices"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func hourlyRecords(start time.Time, n int) []*structs.BackupRecord {
	var out []*structs.BackupRecord
	for i := 0; i < n; i++ {
		t := start.Add(time.Duration(i) * time.Hour)
		out = append(out, &structs.BackupRecord{
			ID:        NewID(t, uint64(i+1)),
			StartedAt: t,
			Status:    structs.BackupStatusComplete,
			Trigger:   structs.BackupTriggerSchedule,
		})
	}
	return out
}

func ids(records []*structs.BackupRecord) []string {
	var out []string
	for _, r := range records {
		out = append(out, r.ID)
	}
	slices.Sort(out)
	return out
}

func TestExpired_GFS(t *testing.T) {
	ci.Parallel(t)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 60 days of hourly backups.
	records := hourlyRecords(start, 60*24)

	expired := Expired(records, &RetentionConfig{KeepHourly: 24, KeepDaily: 14, KeepWeekly: 8, KeepMonthly: 12, MinKeep: 3})
	kept := len(records) - len(expired)
	// 24 hourly; 14 daily of which the newest day is already kept;
	// weekly and monthly add the last backup of older weeks and months.
	must.Between(t, 24+13, kept, 24+13+8+2)

	expiredSet := map[string]bool{}
	for _, r := range expired {
		expiredSet[r.ID] = true
	}
	// The newest 24 are kept.
	for _, r := range records[len(records)-24:] {
		must.False(t, expiredSet[r.ID])
	}
	// The last backup of January (a monthly) is kept.
	must.False(t, expiredSet[records[31*24-1].ID])
	// An arbitrary mid-day backup from 40 days ago is gone.
	must.True(t, expiredSet[records[20*24+12].ID])
}

func TestExpired_PinnedMinKeepFailed(t *testing.T) {
	ci.Parallel(t)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	records := hourlyRecords(start, 10)
	records[0].Pinned = true
	for i := 0; i < 4; i++ {
		records = append(records, &structs.BackupRecord{
			ID:        NewID(start.Add(time.Duration(i)*time.Minute), 100+uint64(i)),
			StartedAt: start.Add(time.Duration(i) * time.Minute),
			Status:    structs.BackupStatusFailed,
		})
	}

	expired := Expired(records, &RetentionConfig{MinKeep: 2, KeepFailed: 1})
	// Kept: the pinned one, the 2 newest, the newest failed one.
	must.Len(t, 7+3, expired)
	for _, r := range expired {
		must.False(t, r.Pinned)
		must.NotEq(t, records[9].ID, r.ID)
		must.NotEq(t, records[8].ID, r.ID)
	}

	// Nothing is deleted while fewer than min_keep exist, however old.
	must.Len(t, 0, Expired(hourlyRecords(start, 3), &RetentionConfig{MinKeep: 3}))
}

func TestCanDelete(t *testing.T) {
	ci.Parallel(t)
	records := hourlyRecords(time.Now().Add(-10*time.Hour), 5)
	r := &RetentionConfig{MinKeep: 2}
	must.NoError(t, CanDelete(records[0], records, r))
	must.ErrorContains(t, CanDelete(records[4], records, r), "min_keep")
	records[0].Pinned = true
	must.ErrorContains(t, CanDelete(records[0], records, r), "pinned")
}

func TestSchedule(t *testing.T) {
	ci.Parallel(t)
	cfg := DefaultConfig()
	cfg.Interval = time.Hour
	cfg.Jitter = 0

	at := time.Date(2026, 10, 24, 14, 20, 0, 0, time.UTC)
	must.Eq(t, time.Date(2026, 10, 24, 15, 0, 0, 0, time.UTC), cfg.NextScheduled(at, 0))
	must.Eq(t, time.Date(2026, 10, 24, 15, 3, 0, 0, time.UTC), cfg.NextScheduled(at, 3*time.Minute))

	cfg.Schedule = "30 2 * * *"
	must.Eq(t, time.Date(2026, 10, 25, 2, 30, 0, 0, time.UTC), cfg.NextScheduled(at, 0))
	must.Eq(t, 24*time.Hour, cfg.period(at, 0))
	cfg.Schedule = ""

	// The offset is stable per region and below the jitter.
	off := RegionOffset("eu", 5*time.Minute)
	must.Eq(t, off, RegionOffset("eu", 5*time.Minute))
	must.Less(t, 5*time.Minute, off)

	// No backups: due now.
	st := cfg.schedule(nil, "eu", at)
	must.Eq(t, at, st.due)

	// Next slot after the last successful scheduled backup; manual backups
	// don't count.
	records := []*structs.BackupRecord{
		{ID: "a", Trigger: structs.BackupTriggerSchedule, Status: structs.BackupStatusComplete, StartedAt: at.Add(-90 * time.Minute)},
		{ID: "b", Trigger: structs.BackupTriggerManual, Status: structs.BackupStatusComplete, StartedAt: at.Add(-time.Minute)},
	}
	st = cfg.schedule(records, "eu", at)
	must.Eq(t, time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC), st.due)

	// A failed attempt is retried after the retry interval.
	records = append(records, &structs.BackupRecord{ID: "c", Trigger: structs.BackupTriggerSchedule,
		Status: structs.BackupStatusFailed, StartedAt: at.Add(-5 * time.Minute)})
	st = cfg.schedule(records, "eu", at)
	must.Eq(t, at.Add(-5*time.Minute+cfg.RetryInterval), st.due)

	status := cfg.ScheduleStatus(records, "eu", at)
	must.Eq(t, "a", status.LastSuccessID)
	must.Eq(t, at.Add(-5*time.Minute), status.LastAttempt)
}

func TestConfig_Validate(t *testing.T) {
	ci.Parallel(t)
	cfg := DefaultConfig()
	must.NoError(t, cfg.Validate())

	cfg.Enabled = true
	must.ErrorContains(t, cfg.Validate(), "at least one destination")

	cfg.Destinations = []*DestinationConfig{{Name: "a", Type: "azure", Prefix: "x/${node}"}, {Name: "b", Type: "s3"}}
	err := cfg.Validate()
	must.ErrorContains(t, err, `unsupported type "azure"`)
	must.ErrorContains(t, err, `destination "b": 1 error occurred:`+"\n\t* missing bucket")
	must.ErrorContains(t, err, "at least one age recipient")
	must.ErrorContains(t, err, "only interpolate ${region}")

	cfg = DefaultConfig()
	cfg.Interval = time.Second
	cfg.PartSize = 1024
	cfg.Schedule = ""
	err = cfg.Validate()
	must.ErrorContains(t, err, "interval")
	must.ErrorContains(t, err, "part_size")

	cfg = DefaultConfig()
	cfg.Schedule = "not a cron"
	must.ErrorContains(t, cfg.Validate(), "invalid schedule")
}

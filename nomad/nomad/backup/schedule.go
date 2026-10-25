// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package backup

import (
	"hash/fnv"
	"time"

	"github.com/hashicorp/cronexpr"
	"github.com/hashicorp/nomad/nomad/structs"
)

// RegionOffset returns the offset of a region's scheduled times, in
// [0, jitter). It is stable, so a region always backs up at the same
// minutes, and differs between regions sharing a store.
func RegionOffset(region string, jitter time.Duration) time.Duration {
	if jitter <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(region))
	return time.Duration(h.Sum64() % uint64(jitter))
}

// NextScheduled returns the first scheduled time strictly after t. Interval
// slots are aligned to the Unix epoch; cron schedules are evaluated in UTC.
// Both are shifted by the offset.
func (c *Config) NextScheduled(t time.Time, offset time.Duration) time.Time {
	base := t.Add(-offset).UTC()
	if c.Schedule != "" {
		expr, err := cronexpr.Parse(c.Schedule)
		if err != nil {
			return time.Time{}
		}
		next := expr.Next(base)
		if next.IsZero() {
			return next
		}
		return next.Add(offset)
	}
	interval := c.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	n := base.UnixNano() / int64(interval)
	return time.Unix(0, (n+1)*int64(interval)).UTC().Add(offset)
}

// period returns the time between two scheduled backups around t.
func (c *Config) period(t time.Time, offset time.Duration) time.Duration {
	if c.Schedule == "" {
		return c.Interval
	}
	next := c.NextScheduled(t, offset)
	if next.IsZero() {
		return 0
	}
	after := c.NextScheduled(next, offset)
	if after.IsZero() {
		return 0
	}
	return after.Sub(next)
}

// scheduleState is the state of the schedule derived from the records of
// the scheduled backups.
type scheduleState struct {
	lastSuccess *structs.BackupRecord
	lastFailed  *structs.BackupRecord
	due         time.Time
}

// schedule computes when the next scheduled backup is due. Only scheduled
// backups count: the next one follows the slot of the last successful one,
// so a new leader neither repeats nor skips a slot. Without any successful
// scheduled backup the first one is due immediately. A failed attempt is
// retried after RetryInterval.
func (c *Config) schedule(records []*structs.BackupRecord, region string, now time.Time) scheduleState {
	var st scheduleState
	for _, b := range records {
		if b.Trigger != structs.BackupTriggerSchedule {
			continue
		}
		if b.Usable() {
			if st.lastSuccess == nil || b.StartedAt.After(st.lastSuccess.StartedAt) {
				st.lastSuccess = b
			}
		} else if st.lastFailed == nil || b.StartedAt.After(st.lastFailed.StartedAt) {
			st.lastFailed = b
		}
	}

	offset := RegionOffset(region, c.Jitter)
	if st.lastSuccess == nil {
		st.due = now
	} else {
		st.due = c.NextScheduled(st.lastSuccess.StartedAt, offset)
	}
	if st.lastFailed != nil && (st.lastSuccess == nil || st.lastFailed.StartedAt.After(st.lastSuccess.StartedAt)) {
		if retry := st.lastFailed.StartedAt.Add(c.RetryInterval); retry.After(st.due) {
			st.due = retry
		}
	}
	return st
}

// ScheduleStatus returns the state of the schedule for the API.
func (c *Config) ScheduleStatus(records []*structs.BackupRecord, region string, now time.Time) *structs.BackupScheduleStatus {
	status := &structs.BackupScheduleStatus{
		Enabled:      c.Enabled && c.Configured(),
		Schedule:     c.Schedule,
		Destinations: c.DestinationNames(),
	}
	if c.Schedule == "" {
		status.Interval = c.Interval
	}

	st := c.schedule(records, region, now)
	if st.lastSuccess != nil {
		status.LastSuccess = st.lastSuccess.CompletedAt
		status.LastSuccessID = st.lastSuccess.ID
		status.LastAttempt = st.lastSuccess.StartedAt
	}
	if st.lastFailed != nil && st.lastFailed.StartedAt.After(status.LastAttempt) {
		status.LastAttempt = st.lastFailed.StartedAt
		status.LastError = st.lastFailed.StatusDescription
	}
	if status.Enabled {
		status.NextDue = st.due
		if period := c.period(now, RegionOffset(region, c.Jitter)); period > 0 && st.lastSuccess != nil {
			status.Overdue = now.Sub(st.lastSuccess.CompletedAt) > 2*period
		}
	}
	return status
}

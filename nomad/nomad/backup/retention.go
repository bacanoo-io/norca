// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package backup

import (
	"fmt"
	"slices"
	"time"

	"github.com/hashicorp/nomad/nomad/structs"
)

// Expired returns the backups that the retention deletes, using the
// grandfather-father-son scheme: the newest backup of each of the last
// KeepHourly hours, KeepDaily days, KeepWeekly ISO weeks and KeepMonthly
// months is kept (in UTC), as well as the MinKeep newest backups and every
// pinned backup. Of the failed backups only the KeepFailed newest records
// are kept.
func Expired(records []*structs.BackupRecord, r *RetentionConfig) []*structs.BackupRecord {
	if r == nil {
		r = DefaultRetention()
	}

	var usable, failed []*structs.BackupRecord
	for _, b := range records {
		if b.Usable() {
			usable = append(usable, b)
		} else {
			failed = append(failed, b)
		}
	}
	newestFirst := func(a, b *structs.BackupRecord) int {
		return b.StartedAt.Compare(a.StartedAt)
	}
	slices.SortStableFunc(usable, newestFirst)
	slices.SortStableFunc(failed, newestFirst)

	keep := make(map[string]struct{})
	for i, b := range usable {
		if b.Pinned || i < r.MinKeep {
			keep[b.ID] = struct{}{}
		}
	}

	classes := []struct {
		count  int
		bucket func(time.Time) string
	}{
		{r.KeepHourly, func(t time.Time) string { return t.Format("2006-01-02T15") }},
		{r.KeepDaily, func(t time.Time) string { return t.Format("2006-01-02") }},
		{r.KeepWeekly, func(t time.Time) string {
			y, w := t.ISOWeek()
			return fmt.Sprintf("%d-W%02d", y, w)
		}},
		{r.KeepMonthly, func(t time.Time) string { return t.Format("2006-01") }},
	}
	for _, class := range classes {
		if class.count <= 0 {
			continue
		}
		seen := make(map[string]struct{})
		for _, b := range usable {
			key := class.bucket(b.StartedAt.UTC())
			if _, ok := seen[key]; ok {
				continue
			}
			if len(seen) == class.count {
				break
			}
			seen[key] = struct{}{}
			keep[b.ID] = struct{}{}
		}
	}

	var expired []*structs.BackupRecord
	for _, b := range usable {
		if _, ok := keep[b.ID]; !ok {
			expired = append(expired, b)
		}
	}
	for i, b := range failed {
		if i >= r.KeepFailed && !b.Pinned {
			expired = append(expired, b)
		}
	}
	return expired
}

// CanDelete reports whether a backup may be deleted manually: pinned
// backups and the MinKeep newest usable backups are protected.
func CanDelete(b *structs.BackupRecord, records []*structs.BackupRecord, r *RetentionConfig) error {
	if b.Pinned {
		return fmt.Errorf("backup %s is pinned; unpin it first", b.ID)
	}
	if !b.Usable() {
		return nil
	}
	if r == nil {
		r = DefaultRetention()
	}
	var newer int
	for _, o := range records {
		if o.Usable() && o.ID != b.ID && o.StartedAt.After(b.StartedAt) {
			newer++
		}
	}
	if newer < r.MinKeep {
		return fmt.Errorf("backup %s is one of the %d newest backups kept by min_keep", b.ID, r.MinKeep)
	}
	return nil
}

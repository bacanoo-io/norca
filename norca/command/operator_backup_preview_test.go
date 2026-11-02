// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

func TestPreviewPage(t *testing.T) {
	ci.Parallel(t)

	for _, tc := range []struct {
		total, page, perPage, first, last int
	}{
		{total: 120, page: 1, perPage: 50, first: 0, last: 50},
		{total: 120, page: 3, perPage: 50, first: 100, last: 120},
		{total: 120, page: 9, perPage: 50, first: 100, last: 120},
		{total: 120, page: 0, perPage: 50, first: 0, last: 50},
		{total: 120, page: 1, perPage: 0, first: 0, last: 50},
		{total: 3, page: 2, perPage: 50, first: 0, last: 3},
	} {
		first, last := previewPage(tc.total, tc.page, tc.perPage)
		must.Eq(t, tc.first, first)
		must.Eq(t, tc.last, last)
	}
}

func TestFormatBackupRestorePreview_Page(t *testing.T) {
	ci.Parallel(t)

	p := &api.BackupRestorePreview{SecretLeasesLost: 2}
	for _, id := range []string{"a", "b", "c"} {
		p.Objects = append(p.Objects, &api.BackupObjectDiff{Type: "quotas", Change: "added", ID: "object-" + id})
	}

	out := formatBackupRestorePreview(p, 2, 1)
	must.StrContains(t, out, "object-b")
	must.StrNotContains(t, out, "object-a")
	must.StrContains(t, out, "Objects 2-2 of 3; next page: -page=3")

	out = formatBackupRestorePreview(p, 3, 1)
	must.StrContains(t, out, "object-c")
	must.StrNotContains(t, out, "next page")
}

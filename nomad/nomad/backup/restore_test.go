// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package backup

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/version"
	"github.com/shoenig/test/must"
)

func TestCheckRestorable(t *testing.T) {
	ci.Parallel(t)

	record := &structs.BackupRecord{ID: "20261024T140000Z-10", Region: "eu", NorcaVersion: version.NorcaVersion}
	must.NoError(t, CheckRestorable(record, "eu", false))

	// Another region only with allowRegionChange.
	must.ErrorContains(t, CheckRestorable(record, "us", false), `region "eu"`)
	must.NoError(t, CheckRestorable(record, "us", true))

	// A backup of a newer Norca is refused; prereleases of this version and
	// older versions are fine.
	newer := *record
	newer.NorcaVersion = "99.0.0"
	must.ErrorContains(t, CheckRestorable(&newer, "eu", true), "older Norca")
	older := *record
	older.NorcaVersion = "0.0.1"
	must.NoError(t, CheckRestorable(&older, "eu", false))
	dev := *record
	dev.NorcaVersion = version.NorcaVersion + "-dev"
	must.NoError(t, CheckRestorable(&dev, "eu", false))
	invalid := *record
	invalid.NorcaVersion = "not a version"
	must.ErrorContains(t, CheckRestorable(&invalid, "eu", false), "invalid Norca version")
}

func TestConfig_Approval(t *testing.T) {
	ci.Parallel(t)

	cfg := DefaultConfig()
	must.True(t, cfg.RequireApproval)
	must.Eq(t, time.Hour, cfg.ApprovalTTL)
	must.NoError(t, cfg.Validate())

	cfg.RequireApproval = false
	must.ErrorContains(t, cfg.Validate(), "require_approval")
	cfg = DefaultConfig()
	cfg.ApprovalTTL = time.Second
	must.ErrorContains(t, cfg.Validate(), "approval_ttl")
}

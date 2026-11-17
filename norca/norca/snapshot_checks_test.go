// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/shoenig/test/must"
)

// Deleting a schedule that takes its snapshots with it needs the rights of
// the snapshot API, as writing it does (review finding SNP-8).
func TestSnapshotSchedule_DeleteChecks(t *testing.T) {
	ci.Parallel(t)
	c, _, cleanup := snapshotTestServer(t, true, true)
	defer cleanup()

	for _, name := range []string{"keep-them", "drop-them", "drop-them-too"} {
		_, err := c.apply("default", volumeSnapshotScheduleKind, name, map[string]any{
			"volume": "db", "schedule": "0 3 * * *", "keep": map[string]any{"last": 7},
			"delete_snapshots": name != "keep-them",
		})
		must.NoError(t, err)
	}

	objects := mock.CreatePolicyAndToken(t, c.s.State(), 1001, "objects",
		`namespace "default" { object "norca.io/VolumeSnapshotSchedule" { capabilities = ["read", "write"] } }`)
	volumes := mock.CreatePolicyAndToken(t, c.s.State(), 1003, "volumes", `namespace "default" {
  object "norca.io/VolumeSnapshotSchedule" { capabilities = ["read", "write"] }
  capabilities = ["csi-write-volume"]
}
plugin { policy = "read" }`)

	noVolume := &certTest{t: t, s: c.s, token: objects.SecretID}
	err := noVolume.delete("default", volumeSnapshotScheduleKind, "drop-them")
	must.Eq(t, 403, errCode(t, err))
	must.ErrorContains(t, err, "csi-write-volume")
	must.False(t, c.get("default", volumeSnapshotScheduleKind, "drop-them").Deleting())

	// Without delete_snapshots nothing is deleted in the storage: the right
	// to write the object is enough.
	must.NoError(t, noVolume.delete("default", volumeSnapshotScheduleKind, "keep-them"))

	writer := &certTest{t: t, s: c.s, token: volumes.SecretID}
	must.NoError(t, writer.delete("default", volumeSnapshotScheduleKind, "drop-them"))
	must.NoError(t, c.delete("default", volumeSnapshotScheduleKind, "drop-them-too"))
}

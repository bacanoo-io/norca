// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The kind of snapshot schedules: CSI snapshots of a volume taken on a cron
// schedule, and the old ones deleted by a retention rule. Registered by the
// servers like the kinds of certificates; the objects are written through
// the generic object API, with the checks of snapshot_checks.go.
//
// Object events are published as they are: the CSI secrets of a schedule
// are in the secret store (secret_path), never in the object.

// volumeSnapshotScheduleKind is the kind of snapshot schedules, in the group
// of the built-in operators.
const volumeSnapshotScheduleKind = "VolumeSnapshotSchedule"

// snapshotScheduleKindSchema is the JSON Schema of snapshot schedules. The
// status is written by the servers only.
const snapshotScheduleKindSchema = `{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "required": ["volume", "schedule", "keep"],
      "properties": {
        "volume": {"type": "string", "minLength": 1, "maxLength": 128},
        "schedule": {"type": "string", "minLength": 1, "maxLength": 128},
        "time_zone": {"type": "string", "maxLength": 64, "default": "UTC"},
        "keep": {
          "type": "object",
          "properties": {
            "last": {"type": "integer", "minimum": 1, "maximum": 500},
            "max_age": {"type": "string", "minLength": 1}
          },
          "additionalProperties": false
        },
        "name_prefix": {"type": "string", "maxLength": 64, "pattern": "^[A-Za-z0-9][A-Za-z0-9._-]*$"},
        "parameters": {"type": "object", "additionalProperties": {"type": "string"}},
        "secret_path": {"type": "string"},
        "delete_snapshots": {"type": "boolean", "default": false},
        "suspend": {"type": "boolean", "default": false}
      },
      "additionalProperties": false
    },
    "status": {
      "type": "object",
      "properties": {
        "plugin": {"type": "string"},
        "lastRun": {"type": "string"},
        "nextRun": {"type": "string"},
        "snapshots": {
          "type": "array",
          "items": {
            "type": "object",
            "properties": {
              "id": {"type": "string"},
              "name": {"type": "string"},
              "volume": {"type": "string"},
              "plugin": {"type": "string"},
              "scheduled": {"type": "string"},
              "createTime": {"type": "string"},
              "sizeBytes": {"type": "integer", "minimum": 0},
              "ready": {"type": "boolean"},
              "deleteError": {"type": "string"}
            }
          }
        },
        "kept": {"type": "integer", "minimum": 0},
        "taken": {"type": "integer", "minimum": 0},
        "deleted": {"type": "integer", "minimum": 0},
        "failures": {"type": "integer", "minimum": 0},
        "retryAt": {"type": "string"},
        "failedRun": {"type": "string"},
        "conditions": {"type": "array", "items": {"type": "object"}},
        "observedGeneration": {"type": "integer", "minimum": 0}
      },
      "additionalProperties": false
    }
  }
}`

// builtinSnapshotKinds returns the kind of snapshot schedules.
func builtinSnapshotKinds() []*structs.ObjectKind {
	return []*structs.ObjectKind{{
		Group:       structs.OperatorsGroup,
		Kind:        volumeSnapshotScheduleKind,
		Description: "CSI snapshots of a volume taken on a schedule, the old ones deleted by a retention rule (built in)",
		Namespaced:  true,
		Schema:      snapshotScheduleKindSchema,
		Rules: []*structs.ObjectKindRule{
			{
				Expression: "!has(oldSelf) || self.spec.volume == oldSelf.spec.volume",
				Message:    "volume can't be changed: delete the schedule and create it again",
			},
			{
				Expression: "has(self.spec.keep.last) || has(self.spec.keep.max_age)",
				Message:    "keep needs last, max_age or both",
			},
		},
		Columns: []*structs.ObjectKindColumn{
			{Name: "Volume", Path: "spec.volume"},
			{Name: "Schedule", Path: "spec.schedule"},
			{Name: "Last Run", Path: "status.lastRun"},
			{Name: "Next Run", Path: "status.nextRun"},
			{Name: "Kept", Path: "status.kept"},
		},
	}}
}

// isSnapshotKind reports whether the kind is the kind of snapshot schedules.
func isSnapshotKind(group, kind string) bool {
	return group == structs.OperatorsGroup && kind == volumeSnapshotScheduleKind
}

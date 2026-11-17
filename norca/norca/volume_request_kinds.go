// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The kind of volume requests: a CSI volume kept as declared by the servers,
// created (or registered), expanded, set back when it was changed by hand,
// and deleted with the request if its policy says so. Registered by the
// servers like the other kinds of the built-in operators; the objects are
// written through the generic object API, with the checks of
// volume_request_checks.go.
//
// Object events are published as they are: the CSI secrets of a request are
// in the secret store (secret_path), never in the object.

// csiVolumeRequestKind is the kind of volume requests, in the group of the
// built-in operators.
const csiVolumeRequestKind = "CSIVolumeRequest"

// The modes of a volume request.
const (
	volumeRequestModeCreate   = "create"
	volumeRequestModeRegister = "register"
)

// The deletion policies of a volume request.
const (
	volumeRequestRetain     = "retain"
	volumeRequestDeregister = "deregister"
	volumeRequestDelete     = "delete"
)

// volumeRequestKindSchema is the JSON Schema of volume requests. capability
// and the topologies are a block or a list of blocks, as HCL gives them, so
// their shape is checked by the write checks. The status is written by the
// servers only.
const volumeRequestKindSchema = `{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "required": ["plugin_id"],
      "properties": {
        "plugin_id": {"type": "string", "minLength": 1, "maxLength": 128},
        "mode": {"type": "string", "enum": ["create", "register"], "default": "create"},
        "external_id": {"type": "string"},
        "display_name": {"type": "string", "maxLength": 256},
        "capacity_min": {"type": "string"},
        "capacity_max": {"type": "string"},
        "capability": {},
        "mount_options": {
          "type": "object",
          "properties": {
            "fs_type": {"type": "string"},
            "mount_flags": {"type": "array", "items": {"type": "string"}}
          },
          "additionalProperties": false
        },
        "topology_request": {
          "type": "object",
          "properties": {"required": {}, "preferred": {}},
          "additionalProperties": false
        },
        "parameters": {"type": "object", "additionalProperties": {"type": "string"}},
        "context": {"type": "object", "additionalProperties": {"type": "string"}},
        "secret_path": {"type": "string"},
        "snapshot_id": {"type": "string"},
        "clone_id": {"type": "string"},
        "deletion_policy": {"type": "string", "enum": ["retain", "deregister", "delete"], "default": "retain"},
        "adopt": {"type": "boolean", "default": false}
      },
      "additionalProperties": false
    },
    "status": {
      "type": "object",
      "properties": {
        "volumeCreateIndex": {"type": "integer", "minimum": 0},
        "creating": {"type": "boolean"},
        "externalID": {"type": "string"},
        "provider": {"type": "string"},
        "capacityBytes": {"type": "integer", "minimum": 0},
        "schedulable": {"type": "boolean"},
        "readAllocs": {"type": "integer", "minimum": 0},
        "writeAllocs": {"type": "integer", "minimum": 0},
        "controllersHealthy": {"type": "integer", "minimum": 0},
        "nodesHealthy": {"type": "integer", "minimum": 0},
        "lastDrift": {"type": "string"},
        "drifts": {"type": "integer", "minimum": 0},
        "failures": {"type": "integer", "minimum": 0},
        "retryAt": {"type": "string"},
        "lastFailure": {"type": "string"},
        "conditions": {"type": "array", "items": {"type": "object"}},
        "observedGeneration": {"type": "integer", "minimum": 0}
      },
      "additionalProperties": false
    }
  }
}`

// builtinVolumeRequestKinds returns the kind of volume requests.
func builtinVolumeRequestKinds() []*structs.ObjectKind {
	immutable := func(field string) *structs.ObjectKindRule {
		return &structs.ObjectKindRule{
			Expression: "!has(oldSelf) || (has(self.spec." + field + ") == has(oldSelf.spec." + field + ") && (!has(self.spec." + field + ") || self.spec." + field + " == oldSelf.spec." + field + "))",
			Message:    field + " can't be changed: delete the request and write it again",
		}
	}
	return []*structs.ObjectKind{{
		Group:       structs.OperatorsGroup,
		Kind:        csiVolumeRequestKind,
		Description: "A CSI volume kept as declared: created or registered, expanded, set back when changed by hand, deleted with its policy (built in)",
		Namespaced:  true,
		Schema:      volumeRequestKindSchema,
		Rules: []*structs.ObjectKindRule{
			immutable("plugin_id"),
			immutable("mode"),
			immutable("external_id"),
			immutable("display_name"),
			immutable("snapshot_id"),
			immutable("clone_id"),
			immutable("parameters"),
			// The topologies of a volume are fixed when it is made.
			immutable("topology_request"),
		},
		Columns: []*structs.ObjectKindColumn{
			{Name: "Plugin", Path: "spec.plugin_id"},
			{Name: "Mode", Path: "spec.mode"},
			{Name: "Capacity", Path: "status.capacityBytes"},
			{Name: "Schedulable", Path: "status.schedulable"},
			{Name: "Policy", Path: "spec.deletion_policy"},
		},
	}}
}

// isVolumeRequestKind reports whether the kind is the kind of volume
// requests.
func isVolumeRequestKind(group, kind string) bool {
	return group == structs.OperatorsGroup && kind == csiVolumeRequestKind
}

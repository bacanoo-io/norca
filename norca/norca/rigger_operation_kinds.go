// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The kind Operation of Rigger (design/rig-integration.md 4.6b): an apply or
// a destroy of a stack that the leader runs with the token of the caller.
// The servers write it, spec and status; it is read and deleted through the
// object API. The full log of the operation is in a stream of the bus
// (rigger_oplog.go); the status holds the plan, the outcomes and the last
// error.

// The actions of an operation.
const (
	riggerOperationApply   = "apply"
	riggerOperationDestroy = "destroy"
)

// The phases of an operation.
const (
	riggerOpRunning     = "Running"
	riggerOpSucceeded   = "Succeeded"
	riggerOpFailed      = "Failed"
	riggerOpCancelled   = "Cancelled"
	riggerOpInterrupted = "Interrupted"
)

// riggerOperationsKept is how many operations of a stack are kept: the
// leader deletes older finished ones.
const riggerOperationsKept = 10

// riggerOperationKindSchema is the JSON Schema of operations. Both parts are
// written by the servers only.
const riggerOperationKindSchema = `{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "properties": {
        "stack": {"type": "string"},
        "action": {"type": "string", "enum": ["apply", "destroy"]},
        "source": {"type": "string"},
        "version": {"type": "string"},
        "filesDigest": {"type": "string"},
        "prune": {"type": "boolean"},
        "adopt": {"type": "boolean"},
        "requestedBy": {"type": "string"}
      },
      "additionalProperties": false
    },
    "status": {
      "type": "object",
      "properties": {
        "phase": {"type": "string", "enum": ["Running", "Succeeded", "Failed", "Cancelled", "Interrupted"]},
        "plan": {"type": "array", "items": {"type": "object"}},
        "outcomes": {"type": "array", "items": {"type": "object"}},
        "error": {"type": "string"},
        "startedAt": {"type": "string"},
        "finishedAt": {"type": "string"},
        "logSubject": {"type": "string"},
        "logStored": {"type": "boolean"},
        "logDropped": {"type": "integer", "minimum": 0},
        "cancelRequested": {"type": "boolean"}
      },
      "additionalProperties": false
    }
  }
}`

// builtinRiggerOperationKinds returns the kind of operations.
func builtinRiggerOperationKinds() []*structs.ObjectKind {
	return []*structs.ObjectKind{{
		Group:       structs.RiggerObjectGroup,
		Kind:        structs.RiggerOperationKind,
		Description: "An apply or a destroy of a stack of Rigger that the servers run; its log is on the bus (built in)",
		Namespaced:  true,
		Schema:      riggerOperationKindSchema,
		Columns: []*structs.ObjectKindColumn{
			{Name: "Stack", Path: "spec.stack"},
			{Name: "Action", Path: "spec.action"},
			{Name: "Phase", Path: "status.phase"},
			{Name: "Started", Path: "status.startedAt"},
			{Name: "Finished", Path: "status.finishedAt"},
		},
	}}
}

// isRiggerOperationKind reports whether the kind is the kind of operations.
func isRiggerOperationKind(group, kind string) bool {
	return group == structs.RiggerObjectGroup && kind == structs.RiggerOperationKind
}

// RiggerOperation is the typed view of a rigger.norca.io/Operation object.
type RiggerOperation struct {
	Namespace string                `json:"namespace"`
	Name      string                `json:"name"`
	Spec      RiggerOperationSpec   `json:"spec"`
	Status    RiggerOperationStatus `json:"status"`
}

// RiggerOperationSpec is what an operation does.
type RiggerOperationSpec struct {
	Stack  string `json:"stack"`
	Action string `json:"action"`
	// Source and Version name a published stack; FilesDigest a local stack
	// sent as files.
	Source      string `json:"source,omitempty"`
	Version     string `json:"version,omitempty"`
	FilesDigest string `json:"filesDigest,omitempty"`
	Prune       bool   `json:"prune,omitempty"`
	Adopt       bool   `json:"adopt,omitempty"`
	// RequestedBy is the accessor of the token that asked for it.
	RequestedBy string `json:"requestedBy,omitempty"`
}

// RiggerOperationStatus is how an operation went.
type RiggerOperationStatus struct {
	Phase string `json:"phase,omitempty"`
	// Plan is the action of each release; Outcomes what the operation did
	// to each.
	Plan       []RiggerOperationRelease `json:"plan,omitempty"`
	Outcomes   []RiggerOperationRelease `json:"outcomes,omitempty"`
	Error      string                   `json:"error,omitempty"`
	StartedAt  string                   `json:"startedAt,omitempty"`
	FinishedAt string                   `json:"finishedAt,omitempty"`
	// LogSubject is the subject of its log on the bus; LogStored reports
	// that the log is stored there (the bus was ready).
	LogSubject string `json:"logSubject,omitempty"`
	LogStored  bool   `json:"logStored,omitempty"`
	// LogDropped counts the records that did not reach the bus.
	LogDropped int64 `json:"logDropped,omitempty"`
	// CancelRequested records a cancel; the operation stops before its
	// next release.
	CancelRequested bool `json:"cancelRequested,omitempty"`
}

// RiggerOperationRelease is a release of an operation: what the plan does
// to it, or what was done.
type RiggerOperationRelease struct {
	Namespace string `json:"namespace"`
	Release   string `json:"release"`
	Action    string `json:"action"`
	Revision  uint64 `json:"revision,omitempty"`
	Skipped   bool   `json:"skipped,omitempty"`
}

// riggerOperationFromObject reads the typed view of an object.
func riggerOperationFromObject(obj *structs.CustomObject) (*RiggerOperation, error) {
	op := &RiggerOperation{Namespace: obj.Namespace, Name: obj.Name}

	for _, part := range []struct {
		from map[string]any
		to   any
	}{{obj.Spec, &op.Spec}, {obj.Status, &op.Status}} {
		if len(part.from) == 0 {
			continue
		}

		data, err := json.Marshal(part.from)
		if err != nil {
			return nil, err
		}

		if err := json.Unmarshal(data, part.to); err != nil {
			return nil, fmt.Errorf("%s %s/%s: %w", structs.RiggerOperationKindRef, obj.Namespace, obj.Name, err)
		}
	}

	return op, nil
}

// checkRiggerOperationWrite refuses a write of an operation through the
// object API: the servers write operations (Rigger.StackApply,
// Rigger.StackDestroy, Rigger.CancelOperation).
func checkRiggerOperationWrite() error {
	return structs.NewErrRPCCodedf(http.StatusForbidden,
		"operations of Rigger are written by the servers (rigger stack apply|destroy, the cancel of an operation)")
}

// isFinishedRiggerOperation reports whether an operation has ended.
func isFinishedRiggerOperation(phase string) bool {
	switch phase {
	case riggerOpSucceeded, riggerOpFailed, riggerOpCancelled, riggerOpInterrupted:
		return true
	default:
		return false
	}
}

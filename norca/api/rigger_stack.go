// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"encoding/json"
	"net/url"
	"strconv"
	"time"
)

// RiggerOperationKind is the kind of an operation of Rigger on a stack.
const RiggerOperationKind = "Operation"

// RiggerStackRequest names a stack and what to do with it: a published stack
// (Source: <namespace>/<name> of the global registry, or a git address;
// Version: a constraint), or a local stack sent as files (Files: the archive
// of rigger Stack.Archive). The namespace of the write options is the
// namespace of the stack.
type RiggerStackRequest struct {
	Source  string
	Version string
	Files   []byte
	// Values are the values of the variables, as a values file (HCL).
	Values string
	Prune  bool
	Adopt  bool
}

// RiggerReleaseRef names a release.
type RiggerReleaseRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// RiggerReleasePlan is what a plan of a stack does to a release.
type RiggerReleasePlan struct {
	Release   RiggerReleaseRef `json:"release"`
	Action    string           `json:"action"`
	Module    RiggerModuleRef  `json:"module"`
	Digest    string           `json:"digest,omitempty"`
	Revision  uint64           `json:"revision,omitempty"`
	Changes   []RiggerChange   `json:"changes,omitempty"`
	DependsOn []string         `json:"depends_on,omitempty"`
	// TakesOver says whose the release is when an apply takes it over,
	// which needs Adopt.
	TakesOver string `json:"takes_over,omitempty"`
}

// RiggerStackPlan is the plan of a stack.
type RiggerStackPlan struct {
	Stack    string              `json:"stack"`
	Region   string              `json:"region"`
	Releases []RiggerReleasePlan `json:"releases"`
	// Generate are the values an apply generates; Secrets what it does to
	// the secrets of the stack.
	Generate []string         `json:"generate,omitempty"`
	Secrets  []map[string]any `json:"secrets,omitempty"`
	Outputs  []map[string]any `json:"outputs"`
}

// RiggerStackPlanResponse is the plan of a stack and its warnings.
type RiggerStackPlanResponse struct {
	Plan     *RiggerStackPlan
	Warnings []string
}

// RiggerRevision is the current revision of a release of a stack.
type RiggerRevision struct {
	Release RiggerReleaseRef `json:"release"`
	Number  uint64           `json:"number"`
	Module  RiggerModuleRef  `json:"module"`
	// Owner is the stack: <namespace>/<name>.
	Owner  string `json:"owner,omitempty"`
	Status string `json:"status"`
}

// RiggerDrift is what differs of a resource of a release.
type RiggerDrift struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

// RiggerReleaseStatus is a release a stack owns and its drift.
type RiggerReleaseStatus struct {
	Revision RiggerRevision `json:"revision"`
	Drift    []RiggerDrift  `json:"drift,omitempty"`
}

// RiggerStackStatus is the status of a stack.
type RiggerStackStatus struct {
	Stack    string
	Releases []RiggerReleaseStatus
}

// RiggerOperation is an apply or a destroy of a stack the servers run.
type RiggerOperation struct {
	Namespace string                `json:"namespace"`
	Name      string                `json:"name"`
	Spec      RiggerOperationSpec   `json:"spec"`
	Status    RiggerOperationStatus `json:"status"`
}

// RiggerOperationSpec is what an operation does.
type RiggerOperationSpec struct {
	Stack       string `json:"stack"`
	Action      string `json:"action"`
	Source      string `json:"source,omitempty"`
	Version     string `json:"version,omitempty"`
	FilesDigest string `json:"filesDigest,omitempty"`
	Prune       bool   `json:"prune,omitempty"`
	Adopt       bool   `json:"adopt,omitempty"`
	RequestedBy string `json:"requestedBy,omitempty"`
}

// RiggerOperationStatus is how an operation went: Phase is Running,
// Succeeded, Failed, Cancelled or Interrupted.
type RiggerOperationStatus struct {
	Phase           string                   `json:"phase,omitempty"`
	Plan            []RiggerOperationRelease `json:"plan,omitempty"`
	Outcomes        []RiggerOperationRelease `json:"outcomes,omitempty"`
	Error           string                   `json:"error,omitempty"`
	StartedAt       string                   `json:"startedAt,omitempty"`
	FinishedAt      string                   `json:"finishedAt,omitempty"`
	LogSubject      string                   `json:"logSubject,omitempty"`
	LogStored       bool                     `json:"logStored,omitempty"`
	LogDropped      int64                    `json:"logDropped,omitempty"`
	CancelRequested bool                     `json:"cancelRequested,omitempty"`
}

// RiggerOperationRelease is a release of an operation.
type RiggerOperationRelease struct {
	Namespace string `json:"namespace"`
	Release   string `json:"release"`
	Action    string `json:"action"`
	Revision  uint64 `json:"revision,omitempty"`
	Skipped   bool   `json:"skipped,omitempty"`
}

// RiggerLogLine is a record of the log of an operation (time, level, msg
// and the attributes of the record) and its sequence.
type RiggerLogLine struct {
	Seq  uint64
	Line map[string]any
}

// RiggerOperationLog is a page of the log of an operation.
type RiggerOperationLog struct {
	Lines []RiggerLogLine
	// Phase is the phase of the operation; Finished reports that no records
	// will follow.
	Phase    string
	Finished bool
	// LogUnavailable says why this page has no records: the log could not
	// be read (a server without a bus), and Finished is of the phase alone.
	LogUnavailable string
}

// StackPlan plans a stack.
func (r *Rigger) StackPlan(req *RiggerStackRequest, w *WriteOptions) (*RiggerStackPlanResponse, *WriteMeta, error) {
	var out RiggerStackPlanResponse

	wm, err := r.client.put("/v1/rigger/stack/plan", req, &out, w)
	if err != nil {
		return nil, nil, err
	}

	return &out, wm, nil
}

// StackStatus reads the releases a stack owns, their revision and drift.
func (r *Rigger) StackStatus(req *RiggerStackRequest, w *WriteOptions) (*RiggerStackStatus, *WriteMeta, error) {
	var out RiggerStackStatus

	wm, err := r.client.put("/v1/rigger/stack/status", req, &out, w)
	if err != nil {
		return nil, nil, err
	}

	return &out, wm, nil
}

// StackApply starts the apply of a stack and returns its operation.
func (r *Rigger) StackApply(req *RiggerStackRequest, w *WriteOptions) (*RiggerOperation, *WriteMeta, error) {
	return r.stackOperation("apply", req, w)
}

// StackDestroy starts the uninstall of the releases of a stack and returns
// its operation.
func (r *Rigger) StackDestroy(req *RiggerStackRequest, w *WriteOptions) (*RiggerOperation, *WriteMeta, error) {
	return r.stackOperation("destroy", req, w)
}

func (r *Rigger) stackOperation(action string, req *RiggerStackRequest, w *WriteOptions) (*RiggerOperation, *WriteMeta, error) {
	var out RiggerOperation

	wm, err := r.client.put("/v1/rigger/stack/"+action, req, &out, w)
	if err != nil {
		return nil, nil, err
	}

	return &out, wm, nil
}

// Operation reads an operation in the namespace of the query options.
func (r *Rigger) Operation(name string, q *QueryOptions) (*RiggerOperation, *QueryMeta, error) {
	obj, qm, err := r.client.Objects().Get(RiggerObjectGroup, RiggerOperationKind, name, q)
	if err != nil {
		return nil, nil, err
	}

	op := &RiggerOperation{Namespace: obj.Namespace, Name: obj.Name}

	for _, part := range []struct {
		from map[string]any
		to   any
	}{{obj.Spec, &op.Spec}, {obj.Status, &op.Status}} {
		data, err := json.Marshal(part.from)
		if err != nil {
			return nil, nil, err
		}

		if err := json.Unmarshal(data, part.to); err != nil {
			return nil, nil, err
		}
	}

	return op, qm, nil
}

// CancelOperation stops an operation before its next release.
func (r *Rigger) CancelOperation(name string, w *WriteOptions) (*RiggerOperation, *WriteMeta, error) {
	var out RiggerOperation

	wm, err := r.client.put("/v1/rigger/operation/"+url.PathEscape(name)+"/cancel", nil, &out, w)
	if err != nil {
		return nil, nil, err
	}

	return &out, wm, nil
}

// OperationLog reads the records of the log of an operation after the
// sequence after, at most limit (0: 500), waiting up to wait for the first.
func (r *Rigger) OperationLog(name string, after uint64, limit int, wait time.Duration, q *QueryOptions) (*RiggerOperationLog, *QueryMeta, error) {
	v := url.Values{}
	if after > 0 {
		v.Set("after", strconv.FormatUint(after, 10))
	}

	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}

	if wait > 0 {
		v.Set("wait", wait.String())
	}

	path := "/v1/rigger/operation/" + url.PathEscape(name) + "/log"
	if len(v) > 0 {
		path += "?" + v.Encode()
	}

	var out RiggerOperationLog

	qm, err := r.client.query(path, &out, q)
	if err != nil {
		return nil, nil, err
	}

	return &out, qm, nil
}

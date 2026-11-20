// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/hashicorp/go-multierror"
)

// Lifecycle of custom objects (operator framework phase 3): finalizers,
// owner references and garbage collection, the object event log, the
// protection of managed objects and jobs, and the scale subresource.

const (
	// TypeCustomObjectEventRecorded is the event stream type of entries
	// added to the event log of an object (topic CustomObject).
	TypeCustomObjectEventRecorded = "CustomObjectEventRecorded"

	// Types of object log events.
	ObjectEventTypeNormal  = "Normal"
	ObjectEventTypeWarning = "Warning"

	// DefaultObjectEventLogLimit is how many events the log of an object
	// keeps; older events are dropped.
	DefaultObjectEventLogLimit = 20

	// MaxObjectEventsPerRequest bounds the events of one write.
	MaxObjectEventsPerRequest = 10

	maxObjectEventReasonLength  = 128
	maxObjectEventMessageLength = 1024
	maxObjectEventMeta          = 16
	maxObjectEventMetaLength    = 256
	maxObjectFinalizers         = 32

	// ObjectOwnerKindJob is the owner kind of objects owned by a job.
	ObjectOwnerKindJob = "Job"

	// JobMetaOwner is the reserved job meta key holding the owner reference
	// of a job created by a controller: "<namespace>/crd/<path form of the
	// group>/<Kind>/<name>" (the form of the controller SDK; see
	// ObjectPathGroup).
	// JobMetaOwnerKind optionally holds the "group/Kind" reference of the
	// owner, which the dashed group can't always be mapped back to.
	JobMetaOwner     = "norca.owner"
	JobMetaOwnerKind = "norca.owner.kind"

	// JobMetaManagedBy is the reserved job meta key naming the identity that
	// manages the job (see ManagedByAllows). Others can't update, stop,
	// revert or scale the job; groups with their own scaling block can still
	// be scaled.
	JobMetaManagedBy = "norca.managed-by"

	// Identity forms of a managed_by value.
	ManagedByWorkloadPrefix = "workload:"
	ManagedByRolePrefix     = "role:"
	ManagedByPolicyPrefix   = "policy:"
	ManagedByTokenPrefix    = "token:"

	// ObjectScaleReason is the reason of the log event written when the
	// replica count of an object changes on the scale subresource.
	// ObjectEventMetaCount and ObjectEventMetaPreviousCount hold the new and
	// the previous count in its meta.
	ObjectScaleReason            = "Scaled"
	ObjectEventMetaCount         = "count"
	ObjectEventMetaPreviousCount = "previous_count"
)

var (
	// validObjectFinalizer allows an optional prefix
	// ("example.com/cleanup").
	validObjectFinalizer = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9._/-]{0,126}[a-zA-Z0-9])?$`)

	// validObjectEventReason is a short CamelCase reason.
	validObjectEventReason = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]*$`)
)

// ValidateObjectFinalizers checks the names of finalizers.
func ValidateObjectFinalizers(finalizers []string) error {
	var mErr *multierror.Error
	if len(finalizers) > maxObjectFinalizers {
		mErr = multierror.Append(mErr, fmt.Errorf("at most %d finalizers are allowed", maxObjectFinalizers))
	}
	for _, f := range finalizers {
		if !validObjectFinalizer.MatchString(f) {
			mErr = multierror.Append(mErr, fmt.Errorf("invalid finalizer %q, must match regex %s", f, validObjectFinalizer))
		}
	}
	return mErr.ErrorOrNil()
}

// ValidateObjectPropagation checks a deletion mode; "" is the default.
func ValidateObjectPropagation(propagation string) error {
	switch propagation {
	case "", ObjectPropagationBackground, ObjectPropagationForeground, ObjectPropagationOrphan:
		return nil
	}
	return fmt.Errorf("invalid deletion propagation %q, must be %q, %q or %q", propagation,
		ObjectPropagationBackground, ObjectPropagationForeground, ObjectPropagationOrphan)
}

// Validate checks an owner reference: its kind, and the form and the length
// of its other fields. They are stored with the object and are not counted
// in its size limit, so each of them is bounded here.
func (r *ObjectOwnerReference) Validate() error {
	if r.Namespace != "" && !validNamespaceName.MatchString(r.Namespace) {
		return fmt.Errorf("invalid namespace, must match regex %s", validNamespaceName)
	}
	if len(r.ID) > maxObjectOwnerIDLength {
		return fmt.Errorf("ID longer than %d", maxObjectOwnerIDLength)
	}
	switch r.Kind {
	case ObjectOwnerKindJob, "Variable":
		if len(r.Name) > maxObjectOwnerNameLength {
			return fmt.Errorf("name longer than %d", maxObjectOwnerNameLength)
		}
		if strings.ContainsFunc(r.Name, func(c rune) bool { return c < 0x20 || c == 0x7f }) {
			return fmt.Errorf("name contains a control character")
		}
		return nil
	}
	group, kind, err := ParseObjectKindRef(r.Kind)
	if err != nil {
		return err
	}
	if err := ValidateObjectKindName(group, kind); err != nil {
		return err
	}
	if !validCustomObjectName.MatchString(r.Name) {
		return fmt.Errorf("invalid name, must match regex %s", validCustomObjectName)
	}
	return nil
}

// IsObject reports whether the reference is to a custom object.
func (r *ObjectOwnerReference) IsObject() bool {
	return r != nil && strings.Contains(r.Kind, "/")
}

// Deleting reports whether the object is marked for deletion.
func (o *CustomObject) Deleting() bool {
	return o.DeletionTime != 0
}

// ObjectEvent is an entry of the event log of an object, written by
// controllers ("Reconciled", "FailedCreate", "BackupSucceeded"). Repeated
// events (the same type, reason, message and reporter as the newest entry)
// are counted instead of added again.
type ObjectEvent struct {
	// Type is "Normal" or "Warning".
	Type string

	// Reason is a short CamelCase reason; Message the human-readable text.
	Reason  string
	Message string

	// Reporter is the identity that wrote the event, set by the server.
	Reporter string

	// Meta holds machine-readable details.
	Meta map[string]string

	// Count is how often the event occurred, between FirstTime and LastTime
	// (UnixNano).
	Count     int
	FirstTime int64
	LastTime  int64

	// Index is the Raft index of the last occurrence.
	Index uint64
}

// Copy returns a copy of the event.
func (e *ObjectEvent) Copy() *ObjectEvent {
	if e == nil {
		return nil
	}
	ne := *e
	ne.Meta = maps.Clone(e.Meta)
	return &ne
}

// Validate checks an event written by a client.
func (e *ObjectEvent) Validate() error {
	var mErr *multierror.Error
	switch e.Type {
	case "", ObjectEventTypeNormal, ObjectEventTypeWarning:
	default:
		mErr = multierror.Append(mErr, fmt.Errorf("invalid event type %q, must be %q or %q", e.Type,
			ObjectEventTypeNormal, ObjectEventTypeWarning))
	}
	if len(e.Reason) > maxObjectEventReasonLength || !validObjectEventReason.MatchString(e.Reason) {
		mErr = multierror.Append(mErr, fmt.Errorf("invalid reason %q, must match regex %s and be at most %d characters",
			e.Reason, validObjectEventReason, maxObjectEventReasonLength))
	}
	if len(e.Message) > maxObjectEventMessageLength {
		mErr = multierror.Append(mErr, fmt.Errorf("message longer than %d", maxObjectEventMessageLength))
	}
	if len(e.Meta) > maxObjectEventMeta {
		mErr = multierror.Append(mErr, fmt.Errorf("at most %d meta entries are allowed", maxObjectEventMeta))
	}
	for k, v := range e.Meta {
		if len(k) > maxObjectEventMetaLength || len(v) > maxObjectEventMetaLength {
			mErr = multierror.Append(mErr, fmt.Errorf("meta %q: key or value longer than %d", k, maxObjectEventMetaLength))
		}
	}
	return mErr.ErrorOrNil()
}

// sameOccurrence reports whether two events are repetitions of each other.
func (e *ObjectEvent) sameOccurrence(o *ObjectEvent) bool {
	return e.Type == o.Type && e.Reason == o.Reason && e.Message == o.Message && e.Reporter == o.Reporter
}

// ObjectEventLog is the event log of one object, newest event last.
type ObjectEventLog struct {
	Group     string
	Kind      string
	Namespace string
	Name      string

	// ObjectID is the ID of the object the log belongs to.
	ObjectID string

	Events []*ObjectEvent

	CreateIndex uint64
	ModifyIndex uint64
}

// KindRef returns the "group/Kind" reference of the object's kind.
func (l *ObjectEventLog) KindRef() string {
	return ObjectKindRef(l.Group, l.Kind)
}

// Copy returns a deep copy of the log.
func (l *ObjectEventLog) Copy() *ObjectEventLog {
	if l == nil {
		return nil
	}
	nl := *l
	nl.Events = make([]*ObjectEvent, len(l.Events))
	for i, e := range l.Events {
		nl.Events[i] = e.Copy()
	}
	return &nl
}

// Append adds events at the Raft index and time (UnixNano), counting
// repetitions of the newest event, and keeps the newest limit events.
func (l *ObjectEventLog) Append(events []*ObjectEvent, index uint64, now int64, limit int) {
	if limit <= 0 {
		limit = DefaultObjectEventLogLimit
	}
	for _, ev := range events {
		e := ev.Copy()
		if e.Type == "" {
			e.Type = ObjectEventTypeNormal
		}
		e.Index = index
		e.LastTime = now
		if n := len(l.Events); n > 0 && l.Events[n-1].sameOccurrence(e) {
			last := l.Events[n-1]
			last.Count++
			last.LastTime = now
			last.Index = index
			last.Meta = e.Meta
			continue
		}
		e.Count = 1
		e.FirstTime = now
		l.Events = append(l.Events, e)
	}
	if over := len(l.Events) - limit; over > 0 {
		l.Events = slices.Clone(l.Events[over:])
	}
}

// ObjectTombstone records the deletion of an object in the background or
// foreground mode, until the garbage collector has deleted what the object
// owned.
type ObjectTombstone struct {
	// ID is the ID of the deleted object.
	ID string

	Group     string
	Kind      string
	Namespace string
	Name      string

	Propagation string

	// DeleteTime (UnixNano) is when the object was deleted.
	DeleteTime int64

	// ApprovedDependents are the dependents of the deleted object that the
	// garbage collector may delete although their deletion destroys data in
	// the storage: see CustomObject.DeletionApproved.
	ApprovedDependents []string

	CreateIndex uint64
}

// KindRef returns the "group/Kind" reference of the deleted object's kind.
func (t *ObjectTombstone) KindRef() string {
	return ObjectKindRef(t.Group, t.Kind)
}

// CustomObjectFinalizersRequest adds and removes finalizers of an object.
// It requires the "manage-finalizers" capability. Finalizers can't be added
// to an object marked for deletion; removing the last finalizer of such an
// object deletes it.
type CustomObjectFinalizersRequest struct {
	Group  string
	Kind   string
	Name   string
	Add    []string
	Remove []string

	// CAS and DryRun as for CustomObjectApplyRequest.
	CAS    *uint64
	DryRun bool

	WriteRequest
}

// CustomObjectEventsRequest adds events to the log of an object. It
// requires the "write-events" capability.
type CustomObjectEventsRequest struct {
	Group  string
	Kind   string
	Name   string
	Events []*ObjectEvent
	WriteRequest
}

// CustomObjectEventsStateRequest is the Raft request of an event log write.
// The events are appended only if the object still has ObjectID.
type CustomObjectEventsStateRequest struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
	ObjectID  string
	Events    []*ObjectEvent

	// Time (UnixNano) is the time of the events; Limit the size of the log
	// (0: DefaultObjectEventLogLimit).
	Time  int64
	Limit int

	WriteRequest
}

// CustomObjectEventsResponse is the response to an event log query.
type CustomObjectEventsResponse struct {
	Events []*ObjectEvent
	QueryMeta
}

// CustomObjectEventsEvent is the event stream payload of new entries of an
// object's event log.
type CustomObjectEventsEvent struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
	Events    []*ObjectEvent
}

// ObjectGCRequest is the Raft request of the garbage collector's
// bookkeeping: it removes the tombstones that have been processed.
type ObjectGCRequest struct {
	DeleteTombstones []string
	WriteRequest
}

// ObjectScale is the scale subresource of an object.
type ObjectScale struct {
	Group     string
	Kind      string
	Namespace string
	Name      string

	// Replicas is the desired count, at the spec path of the kind.
	Replicas int64

	// StatusReplicas is the observed count at the status path, if the kind
	// has one and the status sets it.
	StatusReplicas *int64

	SpecPath   string
	StatusPath string

	Generation  uint64
	ModifyIndex uint64
}

// CustomObjectScaleRequest sets the replica count of an object. It requires
// the "write" capability; the managed_by identity of the object is not
// enforced, so autoscalers can scale objects whose spec is managed by
// another tool. The change is recorded in the object's event log.
type CustomObjectScaleRequest struct {
	Group    string
	Kind     string
	Name     string
	Replicas int64

	// Message and Meta describe the change in the event log.
	Message string
	Meta    map[string]string

	// CAS and DryRun as for CustomObjectApplyRequest.
	CAS    *uint64
	DryRun bool

	WriteRequest
}

// SingleObjectScaleResponse is the response to a scale read.
type SingleObjectScaleResponse struct {
	Scale *ObjectScale
	QueryMeta
}

// CustomObjectScaleResponse is the response to a scale write.
type CustomObjectScaleResponse struct {
	Scale *ObjectScale

	// Unchanged reports that the write did not change the count.
	Unchanged bool

	WriteMeta
}

// workloadJobIDEscaper makes a job ID one path segment of a workload
// identity name: job IDs may contain "/", the separator of the name.
// Namespace and task names can't.
var workloadJobIDEscaper = strings.NewReplacer("%", "%25", "/", "%2F")

// ManagedByWorkload returns the managed_by value that names the workload
// identities of a job: "workload:<namespace>/<job>", with "%" and "/" of
// the job ID written as "%25" and "%2F".
func ManagedByWorkload(namespace, jobID string) string {
	return ManagedByWorkloadPrefix + namespace + "/" + workloadJobIDEscaper.Replace(jobID)
}

// ManagedByAllows reports whether managedBy names an identity (enforced)
// and whether the user is that identity (allowed):
//
//   - "workload:<namespace>/<job>[/<task>]": a workload identity of the job
//     (of the task); "%" and "/" in the job ID are written "%25" and "%2F",
//     so the job "ctrl/x" is not an identity of the job "ctrl",
//   - "role:<name>" and "policy:<name>": a token with the role or policy,
//   - "token:<accessor ID>": the token.
//
// Other values are not enforced.
func ManagedByAllows(managedBy string, user *AdmissionUser) (enforced, allowed bool) {
	if user == nil {
		user = &AdmissionUser{}
	}
	switch {
	case strings.HasPrefix(managedBy, ManagedByWorkloadPrefix):
		return true, user.Type == "workload-identity" &&
			(user.Name == managedBy || strings.HasPrefix(user.Name, managedBy+"/"))
	case strings.HasPrefix(managedBy, ManagedByRolePrefix):
		return true, slices.Contains(user.Roles, strings.TrimPrefix(managedBy, ManagedByRolePrefix))
	case strings.HasPrefix(managedBy, ManagedByPolicyPrefix):
		return true, slices.Contains(user.Policies, strings.TrimPrefix(managedBy, ManagedByPolicyPrefix))
	case strings.HasPrefix(managedBy, ManagedByTokenPrefix):
		return true, user.AccessorID != "" && user.AccessorID == strings.TrimPrefix(managedBy, ManagedByTokenPrefix)
	}
	return false, false
}

// NewObjectManagedError returns the 403 error of a write rejected because
// the target is managed by another identity.
func NewObjectManagedError(what, managedBy string) error {
	return NewErrRPCCodedf(403, "%s is managed by %q; only that identity may modify it", what, managedBy)
}

// objectPathGroupReplacer maps a group to its path form (ObjectPathGroup).
var objectPathGroupReplacer = strings.NewReplacer("-", "--", ".", "-")

// ObjectPathGroup returns the group as written in Variables paths and owner
// references (controller.Kind.PathGroup): dashes are doubled and dots are
// replaced with dashes. Since the labels of a valid group neither start nor
// end with a dash, distinct groups have distinct path forms.
//
// clean room bugfix: dots were replaced with dashes without escaping the
// dashes, so the owner references of objects of groups like
// "db.example.com" and "db-example-com" were the same, and without the
// owner kind meta the garbage collector stopped the jobs of the other kind.
func ObjectPathGroup(group string) string { return objectPathGroupReplacer.Replace(group) }

// ObjectOwnerMetaValue returns the JobMetaOwner value referencing an
// object.
func ObjectOwnerMetaValue(group, kind, namespace, name string) string {
	return namespace + "/crd/" + ObjectPathGroup(group) + "/" + kind + "/" + name
}

// ParseObjectPathGroup returns the group of a path form (ObjectPathGroup):
// a single dash is a dot, and a run of an even number of dashes is half as
// many dashes. Any other run of dashes is no path form of a group.
func ParseObjectPathGroup(path string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(path); {
		if path[i] != '-' {
			b.WriteByte(path[i])
			i++
			continue
		}
		n := 1
		for i+n < len(path) && path[i+n] == '-' {
			n++
		}
		switch {
		case n == 1:
			b.WriteByte('.')
		case n%2 == 0:
			b.WriteString(strings.Repeat("-", n/2))
		default:
			return "", false
		}
		i += n
	}
	group := b.String()
	return group, group != "" && ObjectPathGroup(group) == path
}

// ParseObjectOwnerMetaValue splits a JobMetaOwner value
// (ObjectOwnerMetaValue) into the group, the kind, the namespace and the
// name of the owner it names. The namespace is empty for an owner of a
// cluster-scoped kind. Whether such a kind or object exists is not looked
// at.
func ParseObjectOwnerMetaValue(value string) (group, kind, namespace, name string, err error) {
	parts := strings.Split(value, "/")
	if len(parts) != 5 || parts[1] != "crd" || parts[3] == "" || parts[4] == "" {
		return "", "", "", "", fmt.Errorf("must be <namespace>/crd/<path form of the group>/<Kind>/<name>")
	}
	if parts[0] != "" && !validNamespaceName.MatchString(parts[0]) {
		return "", "", "", "", fmt.Errorf("invalid namespace, must match regex %s", validNamespaceName)
	}
	group, ok := ParseObjectPathGroup(parts[2])
	if !ok {
		return "", "", "", "", fmt.Errorf("%q is not the path form of a group", parts[2])
	}
	return group, parts[3], parts[0], parts[4], nil
}

// objectApprovedJobPrefix starts the entry of a job in a list of approved
// dependents. The other entries are IDs of objects, which are UUIDs.
const objectApprovedJobPrefix = "job:"

// ObjectApprovedJob returns the entry of a job in the list of the dependents
// a deletion was approved for (CustomObject.DeletionApproved). The create
// index tells the job from a later job of its namespace and ID.
func ObjectApprovedJob(namespace, id string, createIndex uint64) string {
	return objectApprovedJobPrefix + namespace + "/" + id + "@" + strconv.FormatUint(createIndex, 10)
}

// JobOwnedBy reports whether the meta of a job references the object as its
// owner.
func JobOwnedBy(meta map[string]string, group, kind, namespace, name string) bool {
	ref, ok := meta[JobMetaOwner]
	if !ok || ref != ObjectOwnerMetaValue(group, kind, namespace, name) {
		return false
	}
	if k, ok := meta[JobMetaOwnerKind]; ok && k != ObjectKindRef(group, kind) {
		return false
	}
	return true
}

// ObjectOwnedBy reports whether the object references the given object as
// one of its owners.
func ObjectOwnedBy(obj *CustomObject, group, kind, namespace, name string) bool {
	return slices.ContainsFunc(obj.Owners, func(r *ObjectOwnerReference) bool {
		return r != nil && r.Kind == ObjectKindRef(group, kind) && r.Namespace == namespace && r.Name == name
	})
}

// ObjectPathValue returns the value at a dotted path ("spec.replicas") of
// the object document {"spec": ..., "status": ...}, or nil.
func ObjectPathValue(obj *CustomObject, path string) any {
	parts := strings.Split(path, ".")
	var cur any
	switch parts[0] {
	case "spec":
		cur = obj.Spec
	case "status":
		cur = obj.Status
	default:
		return nil
	}
	for _, p := range parts[1:] {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

// SetObjectPathValue sets the value at a dotted path ("spec.replicas") in
// the spec or status of the object, creating intermediate maps. Paths with
// list indexes or filters are not supported.
func SetObjectPathValue(obj *CustomObject, path string, value any) error {
	if strings.ContainsAny(path, "[]*?") {
		return fmt.Errorf("path %q is not a plain field path", path)
	}
	parts := strings.Split(path, ".")
	if len(parts) < 2 {
		return fmt.Errorf("path %q has no field", path)
	}
	var root map[string]any
	switch parts[0] {
	case "spec":
		if obj.Spec == nil {
			obj.Spec = map[string]any{}
		}
		root = obj.Spec
	case "status":
		if obj.Status == nil {
			obj.Status = map[string]any{}
		}
		root = obj.Status
	default:
		return fmt.Errorf("path %q must start with spec or status", path)
	}
	cur := root
	for _, p := range parts[1 : len(parts)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			if cur[p] != nil {
				return fmt.Errorf("path %q: %q is not an object", path, p)
			}
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
	cur[parts[len(parts)-1]] = value
	return nil
}

// ObjectCount converts a JSON number to a count. It fails for anything but
// a non-negative integer.
func ObjectCount(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if n < 0 || n != math.Trunc(n) || n > math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	case int64:
		return n, n >= 0
	case int:
		return int64(n), n >= 0
	case uint64:
		return int64(n), n <= math.MaxInt64
	}
	return 0, false
}

// ScaleOf returns the scale subresource of an object of the kind. The
// replica count is 0 if the spec does not set it.
func ScaleOf(kind *ObjectKind, obj *CustomObject) *ObjectScale {
	s := &ObjectScale{
		Group:       obj.Group,
		Kind:        obj.Kind,
		Namespace:   obj.Namespace,
		Name:        obj.Name,
		SpecPath:    kind.Scale.SpecPath,
		StatusPath:  kind.Scale.StatusPath,
		Generation:  obj.Generation,
		ModifyIndex: obj.ModifyIndex,
	}
	s.Replicas, _ = ObjectCount(ObjectPathValue(obj, kind.Scale.SpecPath))
	if kind.Scale.StatusPath != "" {
		if n, ok := ObjectCount(ObjectPathValue(obj, kind.Scale.StatusPath)); ok {
			s.StatusReplicas = &n
		}
	}
	return s
}

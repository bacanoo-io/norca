// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/hashicorp/go-multierror"
)

// Custom objects (operator framework, Kubernetes features spec item 8): an
// ObjectKind registers a type of custom object with a JSON Schema and CEL
// rules, and CustomObjects of that kind are stored in Raft with a spec
// written by users and a status written by controllers.

const (
	// TopicCustomObject is the event stream topic of custom object and
	// object kind changes. Object events have the key "group/Kind/name" and
	// the filter key "group/Kind"; kind events have the key "group/Kind".
	TopicCustomObject Topic = "CustomObject"

	TypeObjectKindUpserted        = "ObjectKindUpserted"
	TypeObjectKindDeleted         = "ObjectKindDeleted"
	TypeCustomObjectUpserted      = "CustomObjectUpserted"
	TypeCustomObjectStatusUpdated = "CustomObjectStatusUpdated"
	TypeCustomObjectDeleted       = "CustomObjectDeleted"
)

const (
	// MaxObjectKindSchemaSize is the maximum size of the JSON Schema of an
	// object kind.
	MaxObjectKindSchemaSize = 256 * 1024

	// MaxCustomObjectSize is the maximum size of the JSON encoding of the
	// spec and status of a custom object together.
	MaxCustomObjectSize = 256 * 1024

	// ObjectStatusObservedGeneration and ObjectStatusConditions are the
	// status fields with a meaning defined by the framework.
	ObjectStatusObservedGeneration = "observedGeneration"
	ObjectStatusConditions         = "conditions"

	// Deletion propagation modes. Background (the default) deletes the object
	// at once and the objects and jobs it owns afterwards; foreground deletes
	// what the object owns first and the object afterwards; orphan leaves
	// what the object owns in place.
	ObjectPropagationBackground = "background"
	ObjectPropagationForeground = "foreground"
	ObjectPropagationOrphan     = "orphan"

	maxObjectKindDescriptionLength = 1024
	maxObjectKindRules             = 64
	maxObjectRuleExpressionLength  = 8192
	maxObjectRuleMessageLength     = 1024
	maxObjectKindColumns           = 16
	maxObjectColumnPathLength      = 256
	maxObjectGroupLength           = 253
	maxObjectLabels                = 64
	maxObjectLabelValueLength      = 256
	maxObjectOwners                = 16
	maxObjectManagedByLength       = 256
)

var (
	// validObjectGroup is a DNS subdomain in lower case.
	validObjectGroup = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

	// validObjectKindName is a CamelCase type name.
	validObjectKindName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,62}$`)

	// validCustomObjectName is the rule for object names. It allows no
	// character that needs escaping in a URL path.
	validCustomObjectName = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9._-]{0,126}[a-zA-Z0-9])?$`)

	// validObjectLabelKey allows an optional prefix ("example.com/team").
	validObjectLabelKey = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9._/-]{0,126}[a-zA-Z0-9])?$`)

	// validObjectColumnName is the rule for display column names.
	validObjectColumnName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9 ._-]{0,63}$`)
)

// ObjectKindRef returns the reference of a kind: "group/Kind".
func ObjectKindRef(group, kind string) string {
	return group + "/" + kind
}

// ParseObjectKindRef splits a "group/Kind" reference.
func ParseObjectKindRef(ref string) (group, kind string, err error) {
	group, kind, ok := strings.Cut(ref, "/")
	if !ok || group == "" || kind == "" || strings.Contains(kind, "/") {
		return "", "", fmt.Errorf("invalid object kind %q, must be <group>/<Kind>", ref)
	}
	return group, kind, nil
}

// ValidateObjectKindName returns an error if the group or kind name is
// invalid.
func ValidateObjectKindName(group, kind string) error {
	var mErr *multierror.Error
	if len(group) > maxObjectGroupLength || !validObjectGroup.MatchString(group) {
		mErr = multierror.Append(mErr, fmt.Errorf("invalid group %q, must be a lower case DNS subdomain", group))
	}
	if !validObjectKindName.MatchString(kind) {
		mErr = multierror.Append(mErr, fmt.Errorf("invalid kind %q, must match regex %s", kind, validObjectKindName))
	}
	return mErr.ErrorOrNil()
}

// ObjectKind is a registered type of custom object.
type ObjectKind struct {
	// Group and Kind identify the type ("db.example.com", "PostgresCluster").
	Group string
	Kind  string

	// Description is the human-friendly description of the type.
	Description string

	// Namespaced reports whether the objects of the kind live in a
	// namespace. Objects of a cluster-scoped kind have an empty namespace
	// and are authorized with the object rules of the namespace "*" ACL
	// block.
	Namespaced bool

	// Schema is a JSON Schema (draft 2020-12 unless it declares another
	// $schema) describing the document {"spec": ..., "status": ...}. Its
	// "default" values are filled in on every write.
	Schema string

	// Rules are CEL validation rules. "self" is the object, "oldSelf" the
	// stored object on update (null on create, has(oldSelf) tests it) and
	// "request" describes the write.
	Rules []*ObjectKindRule

	// Columns are the display columns of the CLI listing and the UI.
	Columns []*ObjectKindColumn

	// Scale describes the scale subresource: the replica count of an
	// object, read and written by the autoscaler and "norca object scale".
	Scale *ObjectKindScale

	// Replicated marks a kind replicated from the authoritative region. It
	// is reserved; replication is not implemented and true is rejected.
	Replicated bool

	// Raft indexes.
	CreateIndex uint64
	ModifyIndex uint64
}

// ObjectKindRule is a CEL validation rule of an object kind. The rule is
// violated when its expression does not evaluate to true.
type ObjectKindRule struct {
	Expression string
	Message    string
}

// ObjectKindColumn is a display column. Path selects a field of the object
// ("spec.version", "status.conditions[?type=='Ready'].status").
type ObjectKindColumn struct {
	Name string
	Path string
}

// ObjectKindScale maps the scale subresource to the fields of the object.
type ObjectKindScale struct {
	SpecPath   string
	StatusPath string
}

// KindRef returns the "group/Kind" reference of the kind.
func (k *ObjectKind) KindRef() string {
	return ObjectKindRef(k.Group, k.Kind)
}

// GetID implements the IDGetter interface required for pagination.
func (k *ObjectKind) GetID() string {
	return k.KindRef()
}

// Validate returns an error if the kind is invalid. The schema and the CEL
// rules are compiled separately by the server.
func (k *ObjectKind) Validate() error {
	var mErr *multierror.Error
	if err := ValidateObjectKindName(k.Group, k.Kind); err != nil {
		mErr = multierror.Append(mErr, err)
	}
	if len(k.Description) > maxObjectKindDescriptionLength {
		mErr = multierror.Append(mErr, fmt.Errorf("description longer than %d", maxObjectKindDescriptionLength))
	}
	switch {
	case strings.TrimSpace(k.Schema) == "":
		mErr = multierror.Append(mErr, fmt.Errorf("missing schema"))
	case len(k.Schema) > MaxObjectKindSchemaSize:
		mErr = multierror.Append(mErr, fmt.Errorf("schema larger than %d bytes", MaxObjectKindSchemaSize))
	}
	if len(k.Rules) > maxObjectKindRules {
		mErr = multierror.Append(mErr, fmt.Errorf("at most %d rules are allowed", maxObjectKindRules))
	}
	for i, rule := range k.Rules {
		switch {
		case rule == nil || strings.TrimSpace(rule.Expression) == "":
			mErr = multierror.Append(mErr, fmt.Errorf("rule %d: missing expression", i))
		case len(rule.Expression) > maxObjectRuleExpressionLength:
			mErr = multierror.Append(mErr, fmt.Errorf("rule %d: expression longer than %d", i, maxObjectRuleExpressionLength))
		case len(rule.Message) > maxObjectRuleMessageLength:
			mErr = multierror.Append(mErr, fmt.Errorf("rule %d: message longer than %d", i, maxObjectRuleMessageLength))
		}
	}
	if len(k.Columns) > maxObjectKindColumns {
		mErr = multierror.Append(mErr, fmt.Errorf("at most %d columns are allowed", maxObjectKindColumns))
	}
	seen := make(map[string]struct{}, len(k.Columns))
	for i, col := range k.Columns {
		if col == nil {
			mErr = multierror.Append(mErr, fmt.Errorf("column %d: empty", i))
			continue
		}
		if !validObjectColumnName.MatchString(col.Name) {
			mErr = multierror.Append(mErr, fmt.Errorf("column %d: invalid name %q, must match regex %s", i, col.Name, validObjectColumnName))
		}
		if _, ok := seen[col.Name]; ok {
			mErr = multierror.Append(mErr, fmt.Errorf("column %d: duplicate name %q", i, col.Name))
		}
		seen[col.Name] = struct{}{}
		if err := validateObjectFieldPath(col.Path); err != nil {
			mErr = multierror.Append(mErr, fmt.Errorf("column %q: %v", col.Name, err))
		}
	}
	if k.Scale != nil {
		if !strings.HasPrefix(k.Scale.SpecPath, "spec.") {
			mErr = multierror.Append(mErr, fmt.Errorf("scale: spec path %q must start with \"spec.\"", k.Scale.SpecPath))
		} else if err := validateObjectFieldPath(k.Scale.SpecPath); err != nil {
			mErr = multierror.Append(mErr, fmt.Errorf("scale: %v", err))
		}
		if k.Scale.StatusPath != "" {
			if !strings.HasPrefix(k.Scale.StatusPath, "status.") {
				mErr = multierror.Append(mErr, fmt.Errorf("scale: status path %q must start with \"status.\"", k.Scale.StatusPath))
			} else if err := validateObjectFieldPath(k.Scale.StatusPath); err != nil {
				mErr = multierror.Append(mErr, fmt.Errorf("scale: %v", err))
			}
		}
	}
	if k.Replicated {
		mErr = multierror.Append(mErr, fmt.Errorf("replicated object kinds are not supported yet"))
	}
	return mErr.ErrorOrNil()
}

// validateObjectFieldPath does a light syntax check of a field path. The
// path is evaluated by the clients.
func validateObjectFieldPath(path string) error {
	switch {
	case path == "":
		return fmt.Errorf("missing path")
	case len(path) > maxObjectColumnPathLength:
		return fmt.Errorf("path longer than %d", maxObjectColumnPathLength)
	case strings.Count(path, "[") != strings.Count(path, "]"):
		return fmt.Errorf("path %q has unbalanced brackets", path)
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("path %q contains a control character", path)
		}
	}
	return nil
}

// Copy returns a deep copy of the kind.
func (k *ObjectKind) Copy() *ObjectKind {
	if k == nil {
		return nil
	}
	nk := new(ObjectKind)
	*nk = *k
	if k.Rules != nil {
		nk.Rules = make([]*ObjectKindRule, len(k.Rules))
		for i, r := range k.Rules {
			if r != nil {
				rc := *r
				nk.Rules[i] = &rc
			}
		}
	}
	if k.Columns != nil {
		nk.Columns = make([]*ObjectKindColumn, len(k.Columns))
		for i, c := range k.Columns {
			if c != nil {
				cc := *c
				nk.Columns[i] = &cc
			}
		}
	}
	if k.Scale != nil {
		sc := *k.Scale
		nk.Scale = &sc
	}
	return nk
}

// CustomObject is an object of a registered kind.
type CustomObject struct {
	// ID is a UUID assigned on creation. It distinguishes an object from a
	// later object with the same name, for owner references.
	ID string

	// Group, Kind, Namespace and Name identify the object. Objects of a
	// cluster-scoped kind have an empty namespace.
	Group     string
	Kind      string
	Namespace string
	Name      string

	// Labels are indexed and can be used to select objects.
	Labels map[string]string

	// ManagedBy names the tool or controller that manages the spec. An
	// identity ("workload:<namespace>/<job>", "role:<name>",
	// "policy:<name>" or "token:<accessor ID>") is enforced: only that
	// identity (and management tokens) may update or delete the object.
	// Other values are free-form, for CEL rules (request.user).
	ManagedBy string

	// Owners are references to the objects this object belongs to. When
	// all its custom object owners are deleted (in the background or
	// foreground mode), the object garbage collector deletes it.
	Owners []*ObjectOwnerReference

	// Finalizers block the deletion of the object: deleting it only sets
	// DeletionTime (UnixNano) and DeletionPropagation, and it disappears
	// when its controllers have removed all finalizers.
	Finalizers          []string
	DeletionTime        int64
	DeletionPropagation string

	// Generation increases on every change of the spec.
	Generation uint64

	// Spec is the desired state, written by users. Status is the observed
	// state, written by controllers on the status endpoint.
	Spec   map[string]any
	Status map[string]any

	// CreateTime and ModifyTime are UnixNano timestamps.
	CreateTime int64
	ModifyTime int64

	// Raft indexes.
	CreateIndex uint64
	ModifyIndex uint64
}

// ObjectOwnerReference references the owner of an object. Kind is a
// "group/Kind" reference for custom objects, or "Job" or "Variable" (stored,
// but not acted on by the garbage collector). An empty namespace means the
// namespace of the object, except for owners of a cluster-scoped kind.
type ObjectOwnerReference struct {
	Kind      string
	Namespace string
	Name      string
	ID        string
}

// KindRef returns the "group/Kind" reference of the kind of the object.
func (o *CustomObject) KindRef() string {
	return ObjectKindRef(o.Group, o.Kind)
}

// GetID implements the IDGetter interface required for pagination.
func (o *CustomObject) GetID() string {
	return o.Name
}

// GetNamespace implements the NamespaceGetter interface.
func (o *CustomObject) GetNamespace() string {
	return o.Namespace
}

// ObservedGeneration returns the generation the status reports, or 0.
func (o *CustomObject) ObservedGeneration() uint64 {
	if o.Status == nil {
		return 0
	}
	switch v := o.Status[ObjectStatusObservedGeneration].(type) {
	case float64:
		if v >= 0 {
			return uint64(v)
		}
	case int64:
		if v >= 0 {
			return uint64(v)
		}
	case uint64:
		return v
	case int:
		if v >= 0 {
			return uint64(v)
		}
	}
	return 0
}

// ValidateMetadata validates the user-writable metadata of the object.
func (o *CustomObject) ValidateMetadata() error {
	var mErr *multierror.Error
	if !validCustomObjectName.MatchString(o.Name) {
		mErr = multierror.Append(mErr, fmt.Errorf("invalid name %q, must match regex %s", o.Name, validCustomObjectName))
	}
	if len(o.Labels) > maxObjectLabels {
		mErr = multierror.Append(mErr, fmt.Errorf("at most %d labels are allowed", maxObjectLabels))
	}
	for k, v := range o.Labels {
		if !validObjectLabelKey.MatchString(k) {
			mErr = multierror.Append(mErr, fmt.Errorf("invalid label key %q, must match regex %s", k, validObjectLabelKey))
		}
		if len(v) > maxObjectLabelValueLength {
			mErr = multierror.Append(mErr, fmt.Errorf("label %q: value longer than %d", k, maxObjectLabelValueLength))
		}
		if strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			mErr = multierror.Append(mErr, fmt.Errorf("label %q: value contains a control character", k))
		}
	}
	if len(o.ManagedBy) > maxObjectManagedByLength {
		mErr = multierror.Append(mErr, fmt.Errorf("managed_by longer than %d", maxObjectManagedByLength))
	}
	if len(o.Owners) > maxObjectOwners {
		mErr = multierror.Append(mErr, fmt.Errorf("at most %d owners are allowed", maxObjectOwners))
	}
	for i, owner := range o.Owners {
		if owner == nil || owner.Kind == "" || owner.Name == "" {
			mErr = multierror.Append(mErr, fmt.Errorf("owner %d: kind and name are required", i))
			continue
		}
		if err := owner.Validate(); err != nil {
			mErr = multierror.Append(mErr, fmt.Errorf("owner %d: %v", i, err))
		}
	}
	if err := ValidateObjectFinalizers(o.Finalizers); err != nil {
		mErr = multierror.Append(mErr, err)
	}
	return mErr.ErrorOrNil()
}

// Copy returns a deep copy of the object.
func (o *CustomObject) Copy() *CustomObject {
	if o == nil {
		return nil
	}
	no := new(CustomObject)
	*no = *o
	no.Labels = maps.Clone(o.Labels)
	no.Finalizers = slices.Clone(o.Finalizers)
	if o.Owners != nil {
		no.Owners = make([]*ObjectOwnerReference, len(o.Owners))
		for i, ref := range o.Owners {
			if ref != nil {
				rc := *ref
				no.Owners[i] = &rc
			}
		}
	}
	no.Spec = CopyObjectMap(o.Spec)
	no.Status = CopyObjectMap(o.Status)
	return no
}

// CopyObjectMap returns a deep copy of a JSON-like map.
func CopyObjectMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	return CopyObjectValue(m).(map[string]any)
}

// CopyObjectValue returns a deep copy of a JSON-like value.
func CopyObjectValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, e := range t {
			m[k] = CopyObjectValue(e)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, e := range t {
			s[i] = CopyObjectValue(e)
		}
		return s
	default:
		return v
	}
}

// NormalizeObjectMap converts a JSON-like map to the canonical form stored
// in the state store: the values encoding/json produces (numbers become
// float64). It fails if the map can't be encoded as JSON.
func NormalizeObjectMap(m map[string]any) (map[string]any, error) {
	if m == nil {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ObjectMapsEqual reports whether two JSON-like maps have the same JSON
// encoding. A nil and an empty map are equal.
func ObjectMapsEqual(a, b map[string]any) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	ab, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ab, bb)
}

// ObjectKindListRequest is used to list object kinds.
type ObjectKindListRequest struct {
	QueryOptions
}

// ObjectKindListResponse is the response to an object kind list request.
type ObjectKindListResponse struct {
	ObjectKinds []*ObjectKind
	QueryMeta
}

// ObjectKindSpecificRequest is used to query a specific object kind.
type ObjectKindSpecificRequest struct {
	Group string
	Kind  string
	QueryOptions
}

// SingleObjectKindResponse is the response to a specific object kind
// request.
type SingleObjectKindResponse struct {
	ObjectKind *ObjectKind
	QueryMeta
}

// ObjectKindUpsertRequest is used to create or update an object kind. It is
// also the Raft request of the upsert.
type ObjectKindUpsertRequest struct {
	ObjectKind *ObjectKind

	// DryRun validates and compiles the kind without writing it.
	DryRun bool

	WriteRequest
}

// ObjectKindUpsertResponse is the response to an object kind upsert.
type ObjectKindUpsertResponse struct {
	ObjectKind *ObjectKind
	WriteMeta
}

// ObjectKindDeleteRequest is used to delete an object kind. It is also the
// Raft request of the deletion.
type ObjectKindDeleteRequest struct {
	Group string
	Kind  string
	WriteRequest
}

// CustomObjectListRequest is used to list the objects of a kind. The
// namespace of the query options selects the namespace ("*" for all).
type CustomObjectListRequest struct {
	Group string
	Kind  string

	// Labels selects the objects that have all these labels.
	Labels map[string]string

	QueryOptions
}

// CustomObjectListResponse is the response to a custom object list request.
type CustomObjectListResponse struct {
	Objects []*CustomObject
	QueryMeta
}

// CustomObjectSpecificRequest is used to query a specific object.
type CustomObjectSpecificRequest struct {
	Group string
	Kind  string
	Name  string
	QueryOptions
}

// SingleCustomObjectResponse is the response to a specific object request.
type SingleCustomObjectResponse struct {
	Object *CustomObject
	QueryMeta
}

// CustomObjectApplyRequest creates or updates the spec and the metadata of
// an object. The status of the request object is ignored.
type CustomObjectApplyRequest struct {
	Object *CustomObject

	// CAS, if set, is the expected modify index of the stored object (0:
	// the object must not exist). A mismatch fails with 409 Conflict.
	CAS *uint64

	// DryRun runs defaulting and validation and returns the resulting
	// object without writing it.
	DryRun bool

	WriteRequest
}

// CustomObjectStatusRequest replaces the status of an object.
type CustomObjectStatusRequest struct {
	Group  string
	Kind   string
	Name   string
	Status map[string]any

	// CAS and DryRun as for CustomObjectApplyRequest.
	CAS    *uint64
	DryRun bool

	WriteRequest
}

// CustomObjectWriteResponse is the response to an object write. Object is
// the object as stored (or as it would be stored, for a dry run).
type CustomObjectWriteResponse struct {
	Object *CustomObject

	// Unchanged reports that the write did not change the object, so
	// nothing was written.
	Unchanged bool

	WriteMeta
}

// CustomObjectDeleteRequest deletes an object.
type CustomObjectDeleteRequest struct {
	Group string
	Kind  string
	Name  string

	// CAS, if set, is the expected modify index of the stored object.
	CAS *uint64

	// Propagation is the deletion mode: "background" (default),
	// "foreground" or "orphan".
	Propagation string

	// DryRun checks the deletion without deleting.
	DryRun bool

	WriteRequest
}

// CustomObjectUpsertStateRequest is the Raft request of an object write.
// The server computed the full object; the state store applies it only if
// the stored object still has ExpectedModifyIndex (0: it must not exist).
type CustomObjectUpsertStateRequest struct {
	Object              *CustomObject
	ExpectedModifyIndex uint64
	WriteRequest
}

// CustomObjectDeleteStateRequest is the Raft request of an object deletion.
// A non-zero ExpectedModifyIndex must match the stored object.
type CustomObjectDeleteStateRequest struct {
	Group               string
	Kind                string
	Namespace           string
	Name                string
	ExpectedModifyIndex uint64

	// Propagation is the deletion mode. For "background" and "foreground"
	// the deletion leaves a tombstone, from which the garbage collector
	// deletes the objects and jobs the object owned; "orphan" and the
	// empty mode (written by servers without the object lifecycle) don't.
	Propagation string

	// DeleteTime (UnixNano) is the time of the deletion recorded in the
	// tombstone.
	DeleteTime int64

	WriteRequest
}

// ObjectKindEvent is the event stream payload of object kind changes.
type ObjectKindEvent struct {
	ObjectKind *ObjectKind
}

// CustomObjectEvent is the event stream payload of custom object changes.
type CustomObjectEvent struct {
	Object *CustomObject
}

// errObjectConflictPrefix starts the message of check-and-set conflicts.
const errObjectConflictPrefix = "object modified concurrently"

// NewObjectConflictError returns the 409 Conflict error of a failed
// check-and-set write.
func NewObjectConflictError(format string, args ...any) error {
	return NewErrRPCCoded(409, errObjectConflictPrefix+": "+fmt.Sprintf(format, args...))
}

// IsObjectConflictError reports whether err is a check-and-set conflict of
// a custom object write.
func IsObjectConflictError(err error) bool {
	code, msg, ok := CodeFromRPCCodedErr(err)
	return ok && code == 409 && strings.HasPrefix(msg, errObjectConflictPrefix)
}

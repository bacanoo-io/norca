// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/mitchellh/mapstructure"
)

// Custom objects of the operator framework: an ObjectKind registers a type
// with a JSON Schema and CEL validation rules, and CustomObjects of that kind
// have a spec written by users and a status written by controllers.

const (
	// TopicCustomObject is the event stream topic of custom object and
	// object kind changes. Subscribe with the key "group/Kind" (globs such
	// as "group/*" are allowed) or "group/Kind/name".
	TopicCustomObject Topic = "CustomObject"

	ObjectKindUpserted        = "ObjectKindUpserted"
	ObjectKindDeleted         = "ObjectKindDeleted"
	CustomObjectUpserted      = "CustomObjectUpserted"
	CustomObjectStatusUpdated = "CustomObjectStatusUpdated"
	CustomObjectDeleted       = "CustomObjectDeleted"

	// ObjectStatusObservedGeneration and ObjectStatusConditions are the
	// status fields with a meaning defined by the framework.
	ObjectStatusObservedGeneration = "observedGeneration"
	ObjectStatusConditions         = "conditions"

	// Standard condition types and statuses.
	ObjectConditionReady       = "Ready"
	ObjectConditionProgressing = "Progressing"
	ObjectConditionDegraded    = "Degraded"
	ObjectConditionTrue        = "True"
	ObjectConditionFalse       = "False"
	ObjectConditionUnknown     = "Unknown"
)

// ErrObjectConflict is wrapped by the errors of writes that failed because
// the object was modified since it was read (409 Conflict). Re-read the
// object and retry.
var ErrObjectConflict = errors.New("object modified concurrently")

// ObjectKind is a registered type of custom object.
type ObjectKind struct {
	Group       string
	Kind        string
	Description string

	// Namespaced reports whether objects of the kind live in namespaces.
	Namespaced bool

	// Schema is the JSON Schema of the document {"spec": ..., "status":
	// ...}, with default values.
	Schema string

	// Rules are CEL validation rules (self, oldSelf, request).
	Rules []*ObjectKindRule

	// Columns are the display columns of listings.
	Columns []*ObjectKindColumn

	// Scale describes the scale subresource (not served yet).
	Scale *ObjectKindScale

	// Replicated is reserved for multi-region replication.
	Replicated bool

	CreateIndex uint64
	ModifyIndex uint64
}

// ObjectKindRule is a CEL validation rule.
type ObjectKindRule struct {
	Expression string
	Message    string
}

// ObjectKindColumn is a display column; Path selects a field of the object,
// e.g. "spec.version" or "status.conditions[?type=='Ready'].status".
type ObjectKindColumn struct {
	Name string
	Path string
}

// ObjectKindScale maps the scale subresource to fields of the object.
type ObjectKindScale struct {
	SpecPath   string
	StatusPath string
}

// KindRef returns the "group/Kind" reference of the kind.
func (k *ObjectKind) KindRef() string {
	return k.Group + "/" + k.Kind
}

// CustomObject is an object of a registered kind.
type CustomObject struct {
	// ID is assigned on creation.
	ID string

	Group     string
	Kind      string
	Namespace string
	Name      string
	Labels    map[string]string

	// ManagedBy names the tool or controller that manages the spec.
	ManagedBy string

	// Owners reference the objects this object belongs to.
	Owners []*ObjectOwnerReference

	// Finalizers and DeletionTime are reserved for the object lifecycle.
	Finalizers   []string
	DeletionTime int64

	// Generation increases on every change of the spec.
	Generation uint64

	Spec   map[string]any
	Status map[string]any

	CreateTime  int64
	ModifyTime  int64
	CreateIndex uint64
	ModifyIndex uint64
}

// ObjectOwnerReference references the owner of an object.
type ObjectOwnerReference struct {
	Kind      string
	Namespace string
	Name      string
	ID        string
}

// ObjectCondition is an entry of the "conditions" list of a status.
type ObjectCondition struct {
	Type               string `json:"type" mapstructure:"type"`
	Status             string `json:"status" mapstructure:"status"`
	Reason             string `json:"reason,omitempty" mapstructure:"reason"`
	Message            string `json:"message,omitempty" mapstructure:"message"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty" mapstructure:"lastTransitionTime"`
}

// KindRef returns the "group/Kind" reference of the kind of the object.
func (o *CustomObject) KindRef() string {
	return o.Group + "/" + o.Kind
}

// ObservedGeneration returns the generation the status reports, or 0.
func (o *CustomObject) ObservedGeneration() uint64 {
	switch v := o.Status[ObjectStatusObservedGeneration].(type) {
	case float64:
		if v >= 0 {
			return uint64(v)
		}
	case int:
		if v >= 0 {
			return uint64(v)
		}
	case int64:
		if v >= 0 {
			return uint64(v)
		}
	case uint64:
		return v
	case json.Number:
		n, _ := strconv.ParseUint(v.String(), 10, 64)
		return n
	}
	return 0
}

// Conditions returns the conditions of the status.
func (o *CustomObject) Conditions() []*ObjectCondition {
	raw, ok := o.Status[ObjectStatusConditions].([]any)
	if !ok {
		return nil
	}
	var out []*ObjectCondition
	for _, r := range raw {
		var c ObjectCondition
		if err := mapstructure.WeakDecode(r, &c); err == nil && c.Type != "" {
			out = append(out, &c)
		}
	}
	return out
}

// Condition returns the condition of the given type, or nil.
func (o *CustomObject) Condition(conditionType string) *ObjectCondition {
	for _, c := range o.Conditions() {
		if c.Type == conditionType {
			return c
		}
	}
	return nil
}

// Document returns the object as display columns and CEL rules see it:
// {"kind", "metadata", "spec", "status"}.
func (o *CustomObject) Document() map[string]any {
	labels := map[string]any{}
	for k, v := range o.Labels {
		labels[k] = v
	}
	doc := map[string]any{
		"kind": o.KindRef(),
		"metadata": map[string]any{
			"name":        o.Name,
			"namespace":   o.Namespace,
			"labels":      labels,
			"managedBy":   o.ManagedBy,
			"generation":  float64(o.Generation),
			"createIndex": float64(o.CreateIndex),
			"modifyIndex": float64(o.ModifyIndex),
		},
		"spec": o.Spec,
	}
	if o.Status != nil {
		doc["status"] = o.Status
	}
	return doc
}

// Field returns the value of a field path of the object, as used by the
// display columns: dotted names ("spec.version"), list indexes
// ("spec.users[0]"), all elements ("spec.users[*].name") and filters
// ("status.conditions[?type=='Ready'].status"). A path with "[*]" or a
// filter returns the list of matching values. A missing field is nil.
func (o *CustomObject) Field(path string) (any, error) {
	return EvalObjectPath(o.Document(), path)
}

// EvalObjectPath evaluates a field path on a JSON-like document. See
// CustomObject.Field.
func EvalObjectPath(doc any, path string) (any, error) {
	values := []any{doc}
	projected := false

	rest := path
	for rest != "" {
		switch {
		case rest[0] == '.':
			rest = rest[1:]
		case rest[0] == '[':
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return nil, fmt.Errorf("invalid path %q: missing ]", path)
			}
			expr := rest[1:end]
			rest = rest[end+1:]

			var next []any
			switch {
			case expr == "*":
				projected = true
				for _, v := range values {
					if list, ok := v.([]any); ok {
						next = append(next, list...)
					}
				}
			case strings.HasPrefix(expr, "?"):
				key, want, err := parsePathFilter(expr[1:])
				if err != nil {
					return nil, fmt.Errorf("invalid path %q: %v", path, err)
				}
				projected = true
				for _, v := range values {
					list, _ := v.([]any)
					for _, e := range list {
						m, ok := e.(map[string]any)
						if ok && formatPathScalar(m[key]) == want {
							next = append(next, e)
						}
					}
				}
			default:
				i, err := strconv.Atoi(expr)
				if err != nil {
					return nil, fmt.Errorf("invalid path %q: bad index %q", path, expr)
				}
				for _, v := range values {
					list, _ := v.([]any)
					idx := i
					if idx < 0 {
						idx += len(list)
					}
					if idx >= 0 && idx < len(list) {
						next = append(next, list[idx])
					}
				}
			}
			values = next
		default:
			end := strings.IndexAny(rest, ".[")
			if end < 0 {
				end = len(rest)
			}
			name := rest[:end]
			rest = rest[end:]

			var next []any
			for _, v := range values {
				switch m := v.(type) {
				case map[string]any:
					if e, ok := m[name]; ok {
						next = append(next, e)
					}
				case map[string]string:
					if e, ok := m[name]; ok {
						next = append(next, e)
					}
				}
			}
			values = next
		}
	}

	if projected {
		return values, nil
	}
	if len(values) == 0 {
		return nil, nil
	}
	return values[0], nil
}

// parsePathFilter parses "key=='value'" (or "key==value").
func parsePathFilter(expr string) (string, string, error) {
	key, value, ok := strings.Cut(expr, "==")
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if !ok || key == "" {
		return "", "", fmt.Errorf("bad filter %q, must be [?key=='value']", expr)
	}
	if len(value) >= 2 && (value[0] == '\'' || value[0] == '"') && value[len(value)-1] == value[0] {
		value = value[1 : len(value)-1]
	}
	return key, value, nil
}

func formatPathScalar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return fmt.Sprint(t)
	}
}

// FormatObjectValue formats a field value for display: lists are joined
// with commas and maps are shown as JSON. A missing value is "".
func FormatObjectValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = FormatObjectValue(e)
		}
		return strings.Join(parts, ",")
	case map[string]any:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	default:
		return formatPathScalar(t)
	}
}

// ObjectWriteOptions are the options of object writes.
type ObjectWriteOptions struct {
	// CAS, if set, is the expected modify index of the stored object (0:
	// the object must not exist). A mismatch fails with an error wrapping
	// ErrObjectConflict.
	CAS *uint64

	// DryRun runs defaulting and validation and returns the result without
	// writing.
	DryRun bool

	// Propagation is the deletion mode; only "background" is supported.
	Propagation string
}

// CASIndex returns a pointer to index, for ObjectWriteOptions.CAS.
func CASIndex(index uint64) *uint64 {
	return &index
}

func (o *ObjectWriteOptions) query() string {
	if o == nil {
		return ""
	}
	v := url.Values{}
	if o.CAS != nil {
		v.Set("cas", strconv.FormatUint(*o.CAS, 10))
	}
	if o.DryRun {
		v.Set("dry_run", "true")
	}
	if o.Propagation != "" {
		v.Set("propagation", o.Propagation)
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// objectWriteError wraps a 409 Conflict response in ErrObjectConflict.
func objectWriteError(err error) error {
	var respErr UnexpectedResponseError
	if errors.As(err, &respErr) && respErr.StatusCode() == http.StatusConflict {
		return fmt.Errorf("%w: %w", ErrObjectConflict, err)
	}
	return err
}

// ObjectKinds is used to access the object kind endpoints.
type ObjectKinds struct {
	client *Client
}

// ObjectKinds returns a handle on the object kind endpoints.
func (c *Client) ObjectKinds() *ObjectKinds {
	return &ObjectKinds{client: c}
}

// List lists the object kinds the caller may read. QueryOptions.Prefix
// filters on the "group/Kind" reference.
func (k *ObjectKinds) List(q *QueryOptions) ([]*ObjectKind, *QueryMeta, error) {
	var resp []*ObjectKind
	qm, err := k.client.query("/v1/object-kinds", &resp, q)
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(resp, func(i, j int) bool { return resp[i].KindRef() < resp[j].KindRef() })
	return resp, qm, nil
}

// Info returns an object kind.
func (k *ObjectKinds) Info(group, kind string, q *QueryOptions) (*ObjectKind, *QueryMeta, error) {
	if group == "" || kind == "" {
		return nil, nil, errors.New("missing object kind group or name")
	}
	var resp ObjectKind
	qm, err := k.client.query("/v1/object-kind/"+url.PathEscape(group)+"/"+url.PathEscape(kind), &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return &resp, qm, nil
}

// Register creates or updates an object kind. With opts.DryRun the kind is
// only validated. It returns the kind as stored.
func (k *ObjectKinds) Register(kind *ObjectKind, opts *ObjectWriteOptions, w *WriteOptions) (*ObjectKind, *WriteMeta, error) {
	if kind == nil || kind.Group == "" || kind.Kind == "" {
		return nil, nil, errors.New("missing object kind group or name")
	}
	var resp ObjectKind
	endpoint := "/v1/object-kind/" + url.PathEscape(kind.Group) + "/" + url.PathEscape(kind.Kind) + opts.query()
	wm, err := k.client.put(endpoint, kind, &resp, w)
	if err != nil {
		return nil, nil, err
	}
	return &resp, wm, nil
}

// Delete deletes an object kind. A kind that still has objects can't be
// deleted.
func (k *ObjectKinds) Delete(group, kind string, w *WriteOptions) (*WriteMeta, error) {
	if group == "" || kind == "" {
		return nil, errors.New("missing object kind group or name")
	}
	return k.client.delete("/v1/object-kind/"+url.PathEscape(group)+"/"+url.PathEscape(kind), nil, nil, w)
}

// Objects is used to access the custom object endpoints.
type Objects struct {
	client *Client
}

// Objects returns a handle on the custom object endpoints.
func (c *Client) Objects() *Objects {
	return &Objects{client: c}
}

// ObjectListOptions are the options of object listings.
type ObjectListOptions struct {
	// Labels selects the objects that have all these labels.
	Labels map[string]string
}

// List lists the objects of a kind in the namespace of the query options
// ("*" for all namespaces). QueryOptions.Filter, PerPage and NextToken
// work as for other listings.
func (o *Objects) List(group, kind string, opts *ObjectListOptions, q *QueryOptions) ([]*CustomObject, *QueryMeta, error) {
	if group == "" || kind == "" {
		return nil, nil, errors.New("missing object kind group or name")
	}
	endpoint := "/v1/objects/" + url.PathEscape(group) + "/" + url.PathEscape(kind)
	if opts != nil && len(opts.Labels) > 0 {
		v := url.Values{}
		for key, value := range opts.Labels {
			v.Add("label", key+"="+value)
		}
		endpoint += "?" + v.Encode()
	}
	var resp []*CustomObject
	qm, err := o.client.query(endpoint, &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return resp, qm, nil
}

// objectPath returns the endpoint of an object.
func objectPath(group, kind, name string) (string, error) {
	if group == "" || kind == "" || name == "" {
		return "", errors.New("missing object kind or name")
	}
	return "/v1/object/" + url.PathEscape(group) + "/" + url.PathEscape(kind) + "/" + url.PathEscape(name), nil
}

// Get returns an object.
func (o *Objects) Get(group, kind, name string, q *QueryOptions) (*CustomObject, *QueryMeta, error) {
	endpoint, err := objectPath(group, kind, name)
	if err != nil {
		return nil, nil, err
	}
	var resp CustomObject
	qm, err := o.client.query(endpoint, &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return &resp, qm, nil
}

// Apply creates or updates the spec and the metadata (labels, managed by,
// owners) of an object; the status is not changed. The server fills in the
// default values of the schema and increases the generation when the spec
// changes. It returns the object as stored.
func (o *Objects) Apply(obj *CustomObject, opts *ObjectWriteOptions, w *WriteOptions) (*CustomObject, *WriteMeta, error) {
	if obj == nil {
		return nil, nil, errors.New("missing object")
	}
	endpoint, err := objectPath(obj.Group, obj.Kind, obj.Name)
	if err != nil {
		return nil, nil, err
	}
	if obj.Namespace != "" && (w == nil || w.Namespace == "") {
		var nw WriteOptions
		if w != nil {
			nw = *w
		}
		nw.Namespace = obj.Namespace
		w = &nw
	}
	var resp CustomObject
	wm, err := o.client.put(endpoint+opts.query(), obj, &resp, w)
	if err != nil {
		return nil, nil, objectWriteError(err)
	}
	return &resp, wm, nil
}

// UpdateStatus replaces the status of an object. It returns the object as
// stored.
func (o *Objects) UpdateStatus(group, kind, name string, status map[string]any, opts *ObjectWriteOptions, w *WriteOptions) (*CustomObject, *WriteMeta, error) {
	endpoint, err := objectPath(group, kind, name)
	if err != nil {
		return nil, nil, err
	}
	var resp CustomObject
	body := map[string]any{"Status": status}
	wm, err := o.client.put(endpoint+"/status"+opts.query(), body, &resp, w)
	if err != nil {
		return nil, nil, objectWriteError(err)
	}
	return &resp, wm, nil
}

// Delete deletes an object. It returns the deleted object.
func (o *Objects) Delete(group, kind, name string, opts *ObjectWriteOptions, w *WriteOptions) (*CustomObject, *WriteMeta, error) {
	endpoint, err := objectPath(group, kind, name)
	if err != nil {
		return nil, nil, err
	}
	var resp CustomObject
	wm, err := o.client.delete(endpoint+opts.query(), nil, &resp, w)
	if err != nil {
		return nil, nil, objectWriteError(err)
	}
	return &resp, wm, nil
}

// ObjectKindEvent is the payload of object kind events.
type ObjectKindEvent struct {
	ObjectKind *ObjectKind
}

// CustomObjectEvent is the payload of custom object events.
type CustomObjectEvent struct {
	Object *CustomObject
}

// CustomObject returns the object of a CustomObject event, or nil for kind
// events.
func (e *Event) CustomObject() (*CustomObject, error) {
	var out CustomObjectEvent
	if err := mapstructure.Decode(e.Payload, &out); err != nil {
		return nil, err
	}
	return out.Object, nil
}

// ObjectKind returns the kind of an ObjectKindUpserted or ObjectKindDeleted
// event, or nil for object events.
func (e *Event) ObjectKind() (*ObjectKind, error) {
	var out ObjectKindEvent
	if err := mapstructure.Decode(e.Payload, &out); err != nil {
		return nil, err
	}
	return out.ObjectKind, nil
}

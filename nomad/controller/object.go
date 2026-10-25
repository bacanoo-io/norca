// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
)

const (
	// PathRoot is the first segment of the Variables path of every custom
	// object.
	PathRoot = "crd"

	// ItemSpec, ItemStatus and ItemLabels are the Variable items holding the
	// JSON encoded spec, status and labels of an object. Other items are
	// preserved but not interpreted.
	ItemSpec   = "spec"
	ItemStatus = "status"
	ItemLabels = "labels"
)

var (
	groupRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
	kindRe  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)
	nameRe  = regexp.MustCompile(`^[A-Za-z0-9_~-]+$`)

	// nativeNameRe is the rule of the server for native object names.
	nativeNameRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9._-]{0,126}[a-zA-Z0-9])?$`)
)

// Kind identifies a custom object type, for example
// Kind{Group: "db.example.com", Kind: "PostgresCluster"}.
type Kind struct {
	// Group is the API group in domain form; dots are allowed here and are
	// replaced with dashes in Variables paths.
	Group string

	// Kind is the type name, conventionally in CamelCase.
	Kind string
}

// String returns the kind as "<group>/<kind>".
func (k Kind) String() string { return k.Group + "/" + k.Kind }

// Validate checks that the kind can be mapped to Variables paths.
func (k Kind) Validate() error {
	if !groupRe.MatchString(k.Group) {
		return fmt.Errorf("invalid group %q: only lowercase letters, digits, dots and dashes are allowed", k.Group)
	}
	if !kindRe.MatchString(k.Kind) {
		return fmt.Errorf("invalid kind %q: must start with a letter and contain only letters, digits, '_' and '-'", k.Kind)
	}
	return nil
}

// PathGroup returns the group as written in Variables paths: dots are
// replaced with dashes ("db.example.com" -> "db-example-com").
func (k Kind) PathGroup() string { return strings.ReplaceAll(k.Group, ".", "-") }

// Prefix returns the Variables path prefix of the objects of the kind,
// including the trailing slash.
func (k Kind) Prefix() string {
	return PathRoot + "/" + k.PathGroup() + "/" + k.Kind + "/"
}

// Path returns the Variables path of the object with the given name.
func (k Kind) Path(name string) string { return k.Prefix() + name }

// WatchPattern returns the Variables event topic key matching every object
// of the kind.
func (k Kind) WatchPattern() string { return k.Prefix() + "*" }

// NameFromPath returns the object name of a Variables path of the kind. It
// returns false if the path does not belong to the kind or the name is
// invalid (for example because it contains further slashes). Names of
// native objects (which may contain dots) are accepted too, since owner
// references use the same path form for both storages.
func (k Kind) NameFromPath(path string) (string, bool) {
	name, ok := strings.CutPrefix(path, k.Prefix())
	if !ok || (ValidateName(name) != nil && ValidateNativeName(name) != nil) {
		return "", false
	}
	return name, true
}

// ValidateName checks that name is a valid object name: a single Variables
// path segment.
func ValidateName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid object name %q: only letters, digits, '-', '_' and '~' are allowed", name)
	}
	return nil
}

// ValidateNativeName checks that name is a valid name of a native object
// (StorageNative): letters, digits, '.', '_' and '-', starting and ending
// with a letter or digit, at most 128 characters.
func ValidateNativeName(name string) error {
	if !nativeNameRe.MatchString(name) {
		return fmt.Errorf("invalid object name %q: must match %s", name, nativeNameRe)
	}
	return nil
}

// ObjectKey identifies an object of a kind.
type ObjectKey struct {
	Namespace string
	Name      string
}

// String returns the key as "<namespace>/<name>".
func (k ObjectKey) String() string { return k.Namespace + "/" + k.Name }

// Object is a custom object, stored in a Variable (StorageVariables) or as
// a native object of a registered kind (StorageNative).
type Object struct {
	Kind Kind
	Key  ObjectKey

	// ID, Generation and ManagedBy are set only for native objects. The
	// server assigns the ID on creation and increases the generation on
	// every spec change.
	ID         string
	Generation uint64
	ManagedBy  string

	// CreateIndex and ModifyIndex are the Raft indexes of the Variable or
	// native object. ModifyIndex is the check-and-set index of every write.
	CreateIndex uint64
	ModifyIndex uint64
	CreateTime  time.Time
	ModifyTime  time.Time

	// Labels are free-form key/value pairs set by the user.
	Labels map[string]string

	// Finalizers, DeletionTime and Owners exist only for native objects.
	// While an object has finalizers, deleting it only sets DeletionTime;
	// the controller cleans up and removes its finalizer (RemoveFinalizer),
	// and the object disappears when no finalizer is left. Owners reference
	// the objects this object belongs to; the server's garbage collector
	// deletes it when they are deleted.
	Finalizers   []string
	DeletionTime time.Time
	Owners       []*api.ObjectOwnerReference

	// Spec is the desired state, written by users. Status is the observed
	// state, written by the controller. Both are JSON documents; empty means
	// not set.
	Spec   json.RawMessage
	Status json.RawMessage

	// Extra holds the Variable items the SDK does not interpret.
	Extra map[string]string
}

// Path returns the Variables path of the object.
func (o *Object) Path() string { return o.Kind.Path(o.Key.Name) }

// DecodeSpec decodes the spec into out. An empty spec leaves out unchanged.
func (o *Object) DecodeSpec(out any) error {
	if len(o.Spec) == 0 {
		return nil
	}
	if err := json.Unmarshal(o.Spec, out); err != nil {
		return fmt.Errorf("invalid spec of %s %s: %w", o.Kind, o.Key, err)
	}
	return nil
}

// DecodeStatus decodes the status into out. An empty status leaves out
// unchanged.
func (o *Object) DecodeStatus(out any) error {
	if len(o.Status) == 0 {
		return nil
	}
	if err := json.Unmarshal(o.Status, out); err != nil {
		return fmt.Errorf("invalid status of %s %s: %w", o.Kind, o.Key, err)
	}
	return nil
}

// SetSpec encodes spec as the object's spec.
func (o *Object) SetSpec(spec any) error {
	b, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	o.Spec = b
	return nil
}

// SetStatus encodes status as the object's status.
func (o *Object) SetStatus(status any) error {
	b, err := json.Marshal(status)
	if err != nil {
		return err
	}
	o.Status = b
	return nil
}

// SpecHash returns a short, stable hash of the spec. It plays the role of
// the generation of Kubernetes objects in this prototype: controllers record
// the hash of the spec they acted on in the status (CommonStatus), so users
// and tools can tell whether the latest spec has been observed. Equivalent
// JSON documents (differing only in whitespace or key order) hash the same.
func (o *Object) SpecHash() string {
	return hashJSON(o.Spec)
}

func hashJSON(doc []byte) string {
	canonical := doc
	var v any
	if err := json.Unmarshal(doc, &v); err == nil {
		// encoding/json sorts map keys, which canonicalizes the document.
		if b, err := json.Marshal(v); err == nil {
			canonical = b
		}
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:8])
}

// ObservedGeneration returns the "observedGeneration" field of the status,
// or 0. Native objects are up to date when it equals Generation.
func (o *Object) ObservedGeneration() uint64 {
	var s struct {
		ObservedGeneration uint64 `json:"observedGeneration"`
	}
	if len(o.Status) == 0 || json.Unmarshal(o.Status, &s) != nil {
		return 0
	}
	return s.ObservedGeneration
}

// Copy returns a deep copy of the object.
func (o *Object) Copy() *Object {
	if o == nil {
		return nil
	}
	out := *o
	out.Labels = maps.Clone(o.Labels)
	out.Extra = maps.Clone(o.Extra)
	out.Finalizers = slices.Clone(o.Finalizers)
	if o.Owners != nil {
		out.Owners = make([]*api.ObjectOwnerReference, len(o.Owners))
		for i, ref := range o.Owners {
			if ref != nil {
				r := *ref
				out.Owners[i] = &r
			}
		}
	}
	out.Spec = bytes.Clone(o.Spec)
	out.Status = bytes.Clone(o.Status)
	return &out
}

// Deleting reports whether the object is marked for deletion: it has
// finalizers and was deleted. The controller cleans up what the object
// owns and removes its finalizer.
func (o *Object) Deleting() bool { return !o.DeletionTime.IsZero() }

// HasFinalizer reports whether the object has the finalizer.
func (o *Object) HasFinalizer(name string) bool { return slices.Contains(o.Finalizers, name) }

// lifecycleChanged reports whether the finalizers or the deletion state of
// an object changed.
func lifecycleChanged(a, b *Object) bool {
	return !a.DeletionTime.Equal(b.DeletionTime) || !slices.Equal(a.Finalizers, b.Finalizers)
}

// specEqual reports whether two objects have the same spec and labels,
// ignoring the formatting of the JSON documents.
func specEqual(a, b *Object) bool {
	return jsonEqual(a.Spec, b.Spec) && maps.Equal(a.Labels, b.Labels) && maps.Equal(a.Extra, b.Extra)
}

func jsonEqual(a, b []byte) bool {
	if bytes.Equal(a, b) {
		return true
	}
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	return hashJSON(a) == hashJSON(b)
}

// ObjectFromVariable decodes a Variable of the kind into an Object.
func ObjectFromVariable(kind Kind, v *api.Variable) (*Object, error) {
	if v == nil {
		return nil, errors.New("nil variable")
	}
	name, ok := kind.NameFromPath(v.Path)
	if !ok {
		return nil, fmt.Errorf("variable %q is not an object of kind %s", v.Path, kind)
	}
	o := &Object{
		Kind:        kind,
		Key:         ObjectKey{Namespace: v.Namespace, Name: name},
		CreateIndex: v.CreateIndex,
		ModifyIndex: v.ModifyIndex,
		CreateTime:  time.Unix(0, v.CreateTime),
		ModifyTime:  time.Unix(0, v.ModifyTime),
	}
	for k, val := range v.Items {
		switch k {
		case ItemSpec:
			if val != "" && !json.Valid([]byte(val)) {
				return nil, fmt.Errorf("the spec of %s %s is not valid JSON", kind, o.Key)
			}
			o.Spec = json.RawMessage(val)
		case ItemStatus:
			// An invalid status is dropped: the controller rewrites it.
			if json.Valid([]byte(val)) {
				o.Status = json.RawMessage(val)
			}
		case ItemLabels:
			if val == "" {
				continue
			}
			if err := json.Unmarshal([]byte(val), &o.Labels); err != nil {
				return nil, fmt.Errorf("the labels of %s %s are invalid: %w", kind, o.Key, err)
			}
		default:
			if o.Extra == nil {
				o.Extra = map[string]string{}
			}
			o.Extra[k] = val
		}
	}
	return o, nil
}

// ToVariable encodes the object as a Variable. The ModifyIndex of the
// Variable is the object's, so it can be written with check-and-set.
func (o *Object) ToVariable() (*api.Variable, error) {
	if err := o.Kind.Validate(); err != nil {
		return nil, err
	}
	if err := ValidateName(o.Key.Name); err != nil {
		return nil, err
	}
	items := make(api.VariableItems, len(o.Extra)+3)
	maps.Copy(items, o.Extra)
	if len(o.Spec) > 0 {
		items[ItemSpec] = string(o.Spec)
	} else {
		// Variables cannot be empty.
		items[ItemSpec] = "{}"
	}
	if len(o.Status) > 0 {
		items[ItemStatus] = string(o.Status)
	}
	if len(o.Labels) > 0 {
		b, err := json.Marshal(o.Labels)
		if err != nil {
			return nil, err
		}
		items[ItemLabels] = string(b)
	}
	return &api.Variable{
		Namespace:   o.Key.Namespace,
		Path:        o.Path(),
		CreateIndex: o.CreateIndex,
		ModifyIndex: o.ModifyIndex,
		Items:       items,
	}, nil
}

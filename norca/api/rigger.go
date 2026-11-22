// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// The registries of Rigger, the package manager: objects of the kind
// rigger.norca.io/Registry, synced when someone asks for it.
const (
	RiggerObjectGroup    = "rigger.norca.io"
	RiggerRegistryKind   = "Registry"
	RiggerGlobalRegistry = "global"
)

// RiggerRegistry is a registry of Rigger: the global registry or a registry
// repository.
type RiggerRegistry struct {
	Name   string               `json:"name"`
	Spec   RiggerRegistrySpec   `json:"spec"`
	Status RiggerRegistryStatus `json:"status"`
	// ModifyIndex is the index of the object, for a check-and-set write.
	ModifyIndex uint64 `json:"-"`
}

// RiggerRegistrySpec is what a registry is.
type RiggerRegistrySpec struct {
	// Type is global (the registry global only) or git.
	Type        string                    `json:"type"`
	Source      string                    `json:"source"`
	TrustedKeys []string                  `json:"trustedKeys,omitempty"`
	AuthSecret  *RiggerRegistryAuthSecret `json:"authSecret,omitempty"`
	// Display is catalog (the UI shows every module and stack) or selected
	// (only those of Selected).
	Display  string              `json:"display,omitempty"`
	Selected []RiggerRegistryRef `json:"selected,omitempty"`
}

// RiggerRegistryAuthSecret names the Variable with the credentials of a
// registry.
type RiggerRegistryAuthSecret struct {
	Namespace string `json:"namespace,omitempty"`
	Path      string `json:"path"`
}

// RiggerRegistryStatus is what the last sync found.
type RiggerRegistryStatus struct {
	Phase          string               `json:"phase,omitempty"`
	Message        string               `json:"message,omitempty"`
	LastSyncedAt   string               `json:"lastSyncedAt,omitempty"`
	LastGoodSyncAt string               `json:"lastGoodSyncAt,omitempty"`
	Source         string               `json:"source,omitempty"`
	Commit         string               `json:"commit,omitempty"`
	Modules        int                  `json:"modules,omitempty"`
	Stacks         int                  `json:"stacks,omitempty"`
	Shown          []RiggerRegistryItem `json:"shown,omitempty"`
	Missing        []RiggerRegistryRef  `json:"missing,omitempty"`
}

// RiggerRegistryRef names a module or stack of a registry: module or stack,
// the namespace (none in a registry repository) and the name.
type RiggerRegistryRef struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

// String is "module acme/nats", "stack web".
func (r RiggerRegistryRef) String() string {
	if r.Namespace == "" {
		return r.Kind + " " + r.Name
	}

	return r.Kind + " " + r.Namespace + "/" + r.Name
}

// RiggerRegistryItem is a module or stack of a registry and its versions,
// the newest first.
type RiggerRegistryItem struct {
	RiggerRegistryRef
	Versions []string `json:"versions"`
}

// RiggerRegistryQuery is a search of a registry.
type RiggerRegistryQuery struct {
	// Kind is module or stack; empty finds both.
	Kind string
	// Text is found in "namespace/name".
	Text          string
	Offset, Limit int
}

// RiggerRegistrySearch is a page of a search.
type RiggerRegistrySearch struct {
	Items []RiggerRegistryItem
	// Total is how many match in all; Commit is the commit searched.
	Total  int
	Commit string
}

// Rigger is the client of the endpoints of Rigger.
type Rigger struct {
	client *Client
}

// Rigger returns the client of the endpoints of Rigger.
func (c *Client) Rigger() *Rigger {
	return &Rigger{client: c}
}

// RiggerRegistryFromObject reads a registry from its object.
func RiggerRegistryFromObject(obj *CustomObject) (*RiggerRegistry, error) {
	reg := &RiggerRegistry{Name: obj.Name, ModifyIndex: obj.ModifyIndex}

	for _, part := range []struct {
		from map[string]any
		to   any
	}{{obj.Spec, &reg.Spec}, {obj.Status, &reg.Status}} {
		if len(part.from) == 0 {
			continue
		}

		data, err := json.Marshal(part.from)
		if err != nil {
			return nil, err
		}

		if err := json.Unmarshal(data, part.to); err != nil {
			return nil, fmt.Errorf("registry %q: %w", obj.Name, err)
		}
	}

	return reg, nil
}

// Registries lists the registries of Rigger.
func (r *Rigger) Registries(q *QueryOptions) ([]*RiggerRegistry, *QueryMeta, error) {
	objs, qm, err := r.client.Objects().List(RiggerObjectGroup, RiggerRegistryKind, nil, q)
	if err != nil {
		return nil, nil, err
	}

	out := make([]*RiggerRegistry, 0, len(objs))

	for _, obj := range objs {
		reg, err := RiggerRegistryFromObject(obj)
		if err != nil {
			return nil, nil, err
		}

		out = append(out, reg)
	}

	return out, qm, nil
}

// Registry reads one registry.
func (r *Rigger) Registry(name string, q *QueryOptions) (*RiggerRegistry, *QueryMeta, error) {
	obj, qm, err := r.client.Objects().Get(RiggerObjectGroup, RiggerRegistryKind, name, q)
	if err != nil {
		return nil, nil, err
	}

	reg, err := RiggerRegistryFromObject(obj)

	return reg, qm, err
}

// SetSelected writes what a registry shows in the UI, with a check-and-set
// on the registry as it was read (reg.ModifyIndex). The UI changes at the
// next sync.
func (r *Rigger) SetSelected(reg *RiggerRegistry, selected []RiggerRegistryRef, w *WriteOptions) (*WriteMeta, error) {
	obj, _, err := r.client.Objects().Get(RiggerObjectGroup, RiggerRegistryKind, reg.Name, nil)
	if err != nil {
		return nil, err
	}

	spec := reg.Spec
	spec.Selected = selected

	data, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}

	obj.Spec = map[string]any{}
	if err := json.Unmarshal(data, &obj.Spec); err != nil {
		return nil, err
	}

	_, wm, err := r.client.Objects().Apply(obj, &ObjectWriteOptions{CAS: &reg.ModifyIndex}, w)

	return wm, err
}

// SyncRegistry fetches the source of a registry now and returns it with
// what the sync found.
func (r *Rigger) SyncRegistry(name string, w *WriteOptions) (*RiggerRegistry, *WriteMeta, error) {
	var out RiggerRegistry

	wm, err := r.client.put("/v1/rigger/registry/"+url.PathEscape(name)+"/sync", nil, &out, w)
	if err != nil {
		return nil, nil, err
	}

	return &out, wm, nil
}

// SearchRegistry searches the index of a registry at the commit of its last
// good sync.
func (r *Rigger) SearchRegistry(name string, query *RiggerRegistryQuery, q *QueryOptions) (*RiggerRegistrySearch, *QueryMeta, error) {
	v := url.Values{}

	if query != nil {
		if query.Kind != "" {
			v.Set("kind", query.Kind)
		}

		if query.Text != "" {
			v.Set("text", query.Text)
		}

		if query.Offset > 0 {
			v.Set("offset", strconv.Itoa(query.Offset))
		}

		if query.Limit > 0 {
			v.Set("limit", strconv.Itoa(query.Limit))
		}
	}

	path := "/v1/rigger/registry/" + url.PathEscape(name) + "/search"
	if len(v) > 0 {
		path += "?" + v.Encode()
	}

	var out RiggerRegistrySearch

	qm, err := r.client.query(path, &out, q)
	if err != nil {
		return nil, nil, err
	}

	return &out, qm, nil
}

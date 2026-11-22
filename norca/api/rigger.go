// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"time"
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
	// Description, Category, Keywords and License are those of the newest
	// version.
	Description string   `json:"description,omitempty"`
	Category    string   `json:"category,omitempty"`
	Keywords    []string `json:"keywords,omitempty"`
	License     string   `json:"license,omitempty"`
}

// RiggerRegistryQuery is a search of a registry.
type RiggerRegistryQuery struct {
	// Kind is module or stack; empty finds both.
	Kind string
	// Text is found in "namespace/name" or in the description.
	Text string
	// Category keeps one category; Keyword the items with the keyword.
	Category, Keyword string
	Offset, Limit     int
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

		if query.Category != "" {
			v.Set("category", query.Category)
		}

		if query.Keyword != "" {
			v.Set("keyword", query.Keyword)
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

// RiggerInstallRequest installs or upgrades one release of a module: one of
// a synced registry (Registry, Module, Version constraint), or a local one
// sent as files (Files, the archive of rigger Module.Archive).
type RiggerInstallRequest struct {
	Release  string
	Registry string
	// Module is <namespace>/<name> in the global registry, <name> in a
	// registry repository.
	Module  string
	Version string
	Files   []byte
	// Values are the values of the variables, as a values file (HCL).
	Values string
	Adopt  bool
	// Timeout bounds the wait until the release is ready; 0 leaves it to
	// the servers.
	Timeout time.Duration
}

// RiggerModuleRef is the module a release records.
type RiggerModuleRef struct {
	Name    string   `json:"name"`
	Source  string   `json:"source"`
	Version string   `json:"version"`
	Ref     string   `json:"ref,omitempty"`
	Digest  string   `json:"digest"`
	Signers []string `json:"signers,omitempty"`
}

// RiggerChange is what an apply would change of a resource.
type RiggerChange struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Action string `json:"action"`
}

// RiggerModuleOrigin is where a module of a registry came from and what
// verified it.
type RiggerModuleOrigin struct {
	Source     string   `json:"source"`
	Version    string   `json:"version"`
	Repository string   `json:"repository,omitempty"`
	Ref        string   `json:"ref,omitempty"`
	Commit     string   `json:"commit,omitempty"`
	Digest     string   `json:"digest"`
	Signers    []string `json:"signers,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
}

// RiggerInstallResponse is the plan of an install, and the revision an apply
// made.
type RiggerInstallResponse struct {
	Action   string
	Module   RiggerModuleRef
	Changes  []RiggerChange
	Revision uint64
	// Origin is nil for a local module.
	Origin   *RiggerModuleOrigin
	Warnings []string
}

// RiggerModuleDetail is what an install form needs of a version of a module
// of a registry.
type RiggerModuleDetail struct {
	Origin     *RiggerModuleOrigin
	Metadata   map[string]any
	Readme     string
	Variables  []map[string]any
	Tabs       []map[string]any
	FormSchema map[string]any
	// Versions are the versions of the module, the newest first.
	Versions []string
	// Meets reports whether the cluster meets the requirements of the
	// version; Unmet lists what it lacks.
	Meets bool
	Unmet []string
	Icon  bool
}

// Install plans (plan) or applies one release of a module.
func (r *Rigger) Install(req *RiggerInstallRequest, plan bool, w *WriteOptions) (*RiggerInstallResponse, *WriteMeta, error) {
	path := "/v1/rigger/install"
	if plan {
		path += "?plan=true"
	}

	var out RiggerInstallResponse

	wm, err := r.client.put(path, req, &out, w)
	if err != nil {
		return nil, nil, err
	}

	return &out, wm, nil
}

// ModuleDetail reads a version of a module of a registry: the newest that
// meets the constraint version (empty: the newest).
func (r *Rigger) ModuleDetail(registry, module, version string, q *QueryOptions) (*RiggerModuleDetail, *QueryMeta, error) {
	var out RiggerModuleDetail

	qm, err := r.client.query(riggerModulePath(registry, "module", module, version), &out, q)
	if err != nil {
		return nil, nil, err
	}

	return &out, qm, nil
}

// Icon reads the icon of a version of a module of a registry: a PNG.
func (r *Rigger) Icon(registry, module, version string, q *QueryOptions) ([]byte, error) {
	body, err := r.client.rawQuery(riggerModulePath(registry, "icon", module, version), q)
	if err != nil {
		return nil, err
	}

	defer body.Close()

	return io.ReadAll(body)
}

func riggerModulePath(registry, action, module, version string) string {
	v := url.Values{"name": {module}}
	if version != "" {
		v.Set("version", version)
	}

	return "/v1/rigger/registry/" + url.PathEscape(registry) + "/" + action + "?" + v.Encode()
}

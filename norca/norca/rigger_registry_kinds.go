// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/rigger/catalog"
	"github.com/bacanoo-io/norca/rigger/fetch"
	"github.com/bacanoo-io/norca/rigger/index"
)

// The kind Registry of Rigger (design/rig-integration.md 4.5): a registry of
// modules and stacks, the global registry or a registry repository. It is
// written through the object API; its status is written by the servers when
// someone asks for a sync (Rigger.SyncRegistry), never on a schedule.
//
// The index of a registry stays on the servers, in the cache of Rigger at
// the commit of the sync; the status holds the commit, the numbers and, for
// a registry that shows only what was selected, the selected modules and
// stacks the sync found (the UI reads the status, so it changes only with a
// sync).

// The types of a Rigger registry.
const (
	riggerRegistryTypeGlobal = "global"
	riggerRegistryTypeGit    = "git"
)

// The display modes of a Rigger registry in the UI.
const (
	// riggerDisplayCatalog shows every module and stack of the registry.
	riggerDisplayCatalog = "catalog"
	// riggerDisplaySelected shows only those selected for the UI; the
	// registry still resolves every module and stack for installs.
	riggerDisplaySelected = "selected"
)

// The phases of the status of a Rigger registry.
const (
	riggerRegistryReady = "Ready"
	riggerRegistryError = "Error"
)

// riggerRegistryKindSchema is the JSON Schema of Rigger registries. The
// status is written by the servers only.
const riggerRegistryKindSchema = `{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "required": ["type", "source"],
      "properties": {
        "type": {"type": "string", "enum": ["global", "git"]},
        "source": {"type": "string", "minLength": 1, "maxLength": 1024},
        "trustedKeys": {"type": "array", "maxItems": 64, "items": {"type": "string", "minLength": 1, "maxLength": 4096}},
        "authSecret": {
          "type": "object",
          "required": ["path"],
          "properties": {
            "namespace": {"type": "string", "maxLength": 128},
            "path": {"type": "string", "minLength": 1, "maxLength": 512}
          },
          "additionalProperties": false
        },
        "display": {"type": "string", "enum": ["catalog", "selected"], "default": "catalog"},
        "selected": {
          "type": "array",
          "maxItems": 1000,
          "items": {
            "type": "object",
            "required": ["kind", "name"],
            "properties": {
              "kind": {"type": "string", "enum": ["module", "stack"]},
              "namespace": {"type": "string", "maxLength": 63},
              "name": {"type": "string", "minLength": 1, "maxLength": 63}
            },
            "additionalProperties": false
          }
        }
      },
      "additionalProperties": false
    },
    "status": {
      "type": "object",
      "properties": {
        "phase": {"type": "string", "enum": ["Ready", "Error"]},
        "message": {"type": "string"},
        "lastSyncedAt": {"type": "string"},
        "lastGoodSyncAt": {"type": "string"},
        "source": {"type": "string"},
        "commit": {"type": "string"},
        "modules": {"type": "integer", "minimum": 0},
        "stacks": {"type": "integer", "minimum": 0},
        "shown": {"type": "array", "items": {"type": "object"}},
        "missing": {"type": "array", "items": {"type": "object"}}
      },
      "additionalProperties": false
    }
  }
}`

// builtinRiggerRegistryKinds returns the kind of Rigger registries.
func builtinRiggerRegistryKinds() []*structs.ObjectKind {
	return []*structs.ObjectKind{{
		Group:       structs.RiggerObjectGroup,
		Kind:        structs.RiggerRegistryKind,
		Description: "Registry of modules and stacks of Rigger, the package manager, synced when asked for (built in)",
		Namespaced:  false,
		Schema:      riggerRegistryKindSchema,
		Columns: []*structs.ObjectKindColumn{
			{Name: "Type", Path: "spec.type"},
			{Name: "Source", Path: "spec.source"},
			{Name: "Display", Path: "spec.display"},
			{Name: "Phase", Path: "status.phase"},
			{Name: "Commit", Path: "status.commit"},
		},
	}}
}

// isRiggerRegistryKind reports whether the kind is the kind of Rigger
// registries.
func isRiggerRegistryKind(group, kind string) bool {
	return group == structs.RiggerObjectGroup && kind == structs.RiggerRegistryKind
}

// RiggerRegistry is the typed view of a rigger.norca.io/Registry object.
type RiggerRegistry struct {
	Name   string               `json:"name"`
	Spec   RiggerRegistrySpec   `json:"spec"`
	Status RiggerRegistryStatus `json:"status"`
}

// RiggerRegistrySpec is what a registry is.
type RiggerRegistrySpec struct {
	// Type is global (the index of the global registry, design 7.3 of
	// Rigger) or git (a registry repository, design 7.4).
	Type string `json:"type"`
	// Source is the git address of the registry.
	Source string `json:"source"`
	// TrustedKeys are public keys the signatures of its versions are
	// checked with at an install (step 4.6).
	TrustedKeys []string `json:"trustedKeys,omitempty"`
	// AuthSecret names the Variable with the credentials of a private
	// source: username and password (or token) for https, ssh_private_key
	// and known_hosts for ssh.
	AuthSecret *RiggerRegistryAuthSecret `json:"authSecret,omitempty"`
	// Display is catalog (the UI shows every module and stack) or selected
	// (only those of Selected).
	Display  string        `json:"display,omitempty"`
	Selected []catalog.Ref `json:"selected,omitempty"`
}

// RiggerRegistryAuthSecret names a Variable with the credentials of a
// registry.
type RiggerRegistryAuthSecret struct {
	Namespace string `json:"namespace,omitempty"`
	Path      string `json:"path"`
}

// RiggerRegistryStatus is what the last sync found.
type RiggerRegistryStatus struct {
	// Phase is Ready or Error; empty for a registry never synced.
	Phase   string `json:"phase,omitempty"`
	Message string `json:"message,omitempty"`
	// LastSyncedAt is the last sync, LastGoodSyncAt the last one that
	// worked: the commit and the numbers are its, also after failed syncs.
	LastSyncedAt   string `json:"lastSyncedAt,omitempty"`
	LastGoodSyncAt string `json:"lastGoodSyncAt,omitempty"`
	// Source and Commit are the source and commit of the last good sync:
	// what installs and searches read.
	Source  string `json:"source,omitempty"`
	Commit  string `json:"commit,omitempty"`
	Modules int    `json:"modules,omitempty"`
	Stacks  int    `json:"stacks,omitempty"`
	// Shown are the selected modules and stacks the sync found, with their
	// versions; Missing those it did not (display selected only).
	Shown   []catalog.Item `json:"shown,omitempty"`
	Missing []catalog.Ref  `json:"missing,omitempty"`
}

// riggerRegistryFromObject reads the typed view of an object.
func riggerRegistryFromObject(obj *structs.CustomObject) (*RiggerRegistry, error) {
	reg := &RiggerRegistry{Name: obj.Name}

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
			return nil, fmt.Errorf("%s %q: %w", structs.RiggerRegistryKindRef, obj.Name, err)
		}
	}

	if reg.Spec.Display == "" {
		reg.Spec.Display = riggerDisplayCatalog
	}

	return reg, nil
}

// objectMap returns a part of the typed view as the map of an object.
func objectMap(v any) (map[string]any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}

	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}

	return out, nil
}

// checkRiggerRegistryApply checks a Rigger registry that is written through
// the object API, beyond its schema: only the registry global is of the
// type global, the selected names fit its type, and a token that names an
// authSecret can read that Variable (so that someone who may only write
// registries can't send the credentials of others to a source of their
// choosing), as can one that changes a registry that has one: without it
// the registry would offer what was synced with it to who can't read it.
func (s *Server) checkRiggerRegistryApply(aclObj *acl.ACL, existing, obj *structs.CustomObject) error {
	reg, err := riggerRegistryFromObject(obj)
	if err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}

	global := obj.Name == structs.RiggerGlobalRegistry
	if global != (reg.Spec.Type == riggerRegistryTypeGlobal) {
		return structs.NewErrRPCCodedf(http.StatusBadRequest,
			"only the registry %q is of the type %q, and it is of no other type", structs.RiggerGlobalRegistry, riggerRegistryTypeGlobal)
	}

	for _, ref := range reg.Spec.Selected {
		if err := checkSelectedRef(reg.Spec.Type, ref); err != nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "selected %s: %v", ref, err)
		}
	}

	if existing != nil {
		was, err := riggerRegistryFromObject(existing)
		if err != nil {
			return err
		}

		if a := was.Spec.AuthSecret; a != nil && !canReadRiggerAuth(aclObj, a) {
			return errRiggerAuthUnreadable(a)
		}
	}

	return s.checkRiggerRegistryAuth(aclObj, reg.Spec)
}

// checkSelectedRef checks a selected module or stack: in the global
// registry it has a namespace, in a registry repository none.
func checkSelectedRef(registryType string, ref catalog.Ref) error {
	if !index.ValidName(ref.Name) {
		return fmt.Errorf("invalid name %q", ref.Name)
	}

	switch {
	case registryType == riggerRegistryTypeGlobal && !index.ValidName(ref.Namespace):
		return fmt.Errorf("a module or stack of the global registry has a namespace: <namespace>/<name>")
	case registryType == riggerRegistryTypeGit && ref.Namespace != "":
		return fmt.Errorf("a registry repository has no namespaces")
	}

	return nil
}

// checkRiggerRegistryAuth checks the authSecret of a registry: the token can
// read the Variable, it exists and holds credentials that fit the source.
func (s *Server) checkRiggerRegistryAuth(aclObj *acl.ACL, spec RiggerRegistrySpec) error {
	a := spec.AuthSecret
	if a == nil {
		return nil
	}

	if !canReadRiggerAuth(aclObj, a) {
		return errRiggerAuthUnreadable(a)
	}

	creds, err := s.riggerRegistryCredentials(a)
	if err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "authSecret: %v", err)
	}

	if err := checkCredentialsFit(spec.Source, creds); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "authSecret %q: %v", a.Path, err)
	}

	return nil
}

// errRiggerAuthUnreadable is the error of a write of a registry by someone
// who can't read the Variable of its authSecret.
func errRiggerAuthUnreadable(a *RiggerRegistryAuthSecret) error {
	return structs.NewErrRPCCodedf(http.StatusForbidden,
		"authSecret: the token can't read the Variable %q in the namespace %q", a.Path, riggerAuthNamespace(a))
}

func riggerAuthNamespace(a *RiggerRegistryAuthSecret) string {
	if ns := strings.TrimSpace(a.Namespace); ns != "" {
		return ns
	}

	return structs.DefaultNamespace
}

// checkCredentialsFit checks that credentials fit a source: an ssh source
// needs a key and the known hosts, an https one a password or token, and a
// source on the file system of the servers takes none.
func checkCredentialsFit(source string, c *fetch.Credentials) error {
	switch {
	case strings.HasPrefix(source, "/") || strings.HasPrefix(source, "file://"):
		return fmt.Errorf("a source on the servers takes no credentials")
	case fetch.SSHRepository(source):
		if len(c.SSHKey) == 0 || len(c.KnownHosts) == 0 {
			return fmt.Errorf("an ssh source needs the items ssh_private_key and known_hosts")
		}
	case strings.HasPrefix(source, "http://"):
		return fmt.Errorf("credentials are sent over https or ssh only")
	default:
		if c.Password == "" {
			return fmt.Errorf("an https source needs the item password or token")
		}
	}

	return nil
}

// checkRiggerRegistryStatusWrite refuses a write of the status of a Rigger
// registry through the object API.
func checkRiggerRegistryStatusWrite() error {
	return structs.NewErrRPCCodedf(http.StatusForbidden,
		"the status of a Rigger registry is written by the servers, at a sync (rigger registry sync)")
}

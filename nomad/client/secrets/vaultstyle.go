// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/nomad/nomad/structs"
)

// Vault-style lookups (docs/design/secrets-plan.md, "Vault compatibility"):
// the Vault "secret" template function and the reads through the client's
// Vault-compatible endpoint, when the agent has no Vault integration. They
// follow the resolution order of every lookup (VaultStyle set); when they
// resolve to the built-in store, the vault_kv rules map the Vault path onto a
// secret of the store, and the value is returned in the response shape of the
// Vault KV engine (v1, v2 or the v2 metadata) so that templates and Vault
// clients read it unchanged.

// kvV1LeaseDuration is the lease duration of KV v1 responses, in seconds:
// Vault's default refresh hint of the KV v1 engine (32 days).
const kvV1LeaseDuration = 2764800

// VaultKV is a Vault-style KV response built from a secret. It never goes to
// logs or events.
type VaultKV struct {
	// Data is the "data" field of the Vault response: the items (KV v1),
	// "data" and "metadata" (KV v2), or the metadata of a KV v2 metadata
	// read.
	Data map[string]any

	// LeaseDuration is the lease duration of the response, in seconds.
	LeaseDuration int

	// Version identifies the value at the provider for change detection
	// (the current version for metadata reads); empty for providers
	// without versions.
	Version string
}

// Copy returns a deep copy of the response, so that callers (templates) can
// not change the cached one.
func (kv *VaultKV) Copy() *VaultKV {
	if kv == nil {
		return nil
	}
	out := *kv
	out.Data, _ = copyJSONValue(kv.Data).(map[string]any)
	return &out
}

// Fingerprint is a hash of the response data for change detection. It is
// only kept in memory, like Fingerprint.
func (kv *VaultKV) Fingerprint() string {
	b, _ := json.Marshal(kv.Data)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func copyJSONValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = copyJSONValue(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = copyJSONValue(e)
		}
		return out
	default:
		return v
	}
}

// VaultReader serves Vault-style lookups.
type VaultReader struct {
	Registry *Registry
	Mapper   *VaultMapper
	Auditor  Auditor
}

// VaultLookup is a resolved Vault-style lookup.
type VaultLookup struct {
	// VaultPath is the Vault path, without a leading slash and query.
	VaultPath string

	// Version is the pinned version of a KV v2 read (?version=n), 0 for
	// the latest.
	Version uint64

	Resolution *Resolution

	// Mapped is the secret of the built-in store the path maps to; nil
	// for other providers, which read the Vault path as it is.
	Mapped *VaultPath

	// KVVersion is the KV engine version of the response shape, Metadata
	// marks a KV v2 metadata read.
	KVVersion int
	Metadata  bool
}

// ParseVaultRef splits a Vault path of a template call or a request into the
// path and the pinned version of its "?version=n" query.
func ParseVaultRef(ref string) (string, uint64, error) {
	p, query, _ := strings.Cut(strings.TrimPrefix(ref, "/"), "?")
	if query == "" {
		return p, 0, nil
	}
	q, err := url.ParseQuery(query)
	if err != nil {
		return "", 0, NewError(structs.SecretProviderNorca, KindInvalid, fmt.Errorf("invalid query of Vault path %q", ref))
	}
	var version uint64
	for k, v := range q {
		if k != "version" || len(v) != 1 {
			return "", 0, NewError(structs.SecretProviderNorca, KindInvalid,
				fmt.Errorf("query parameter %q of Vault path %q is not supported by the built-in store", k, p))
		}
		version, err = strconv.ParseUint(v[0], 10, 64)
		if err != nil {
			return "", 0, NewError(structs.SecretProviderNorca, KindInvalid, fmt.Errorf("invalid version %q", v[0]))
		}
	}
	return p, version, nil
}

// Resolve resolves the provider of a Vault-style lookup and, for the
// built-in store, maps its path. Paths the built-in store can't serve fail
// here, with an error that is never resolved around.
func (r *VaultReader) Resolve(l *Lookup, version uint64) (*VaultLookup, error) {
	l.VaultStyle = true
	res, err := r.Registry.Resolve(l)
	if err != nil {
		return nil, err
	}
	vl := &VaultLookup{VaultPath: l.Path, Version: version, Resolution: res}
	if res.Provider.Type() == structs.SecretProviderNorca {
		mapped, err := r.Mapper.Map(l.Path)
		if err != nil {
			return nil, err
		}
		vl.Mapped, vl.KVVersion, vl.Metadata = mapped, mapped.KVVersion, mapped.Metadata
	} else {
		vl.KVVersion = r.Mapper.KVVersion(l.Path)
	}
	if version != 0 && (vl.KVVersion != 2 || vl.Metadata) {
		return nil, NewError(res.Provider.Name(), KindInvalid,
			fmt.Errorf("Vault path %q: versions can only be read from KV v2 data paths", l.Path))
	}
	return vl, nil
}

// Request returns the provider request of the lookup.
func (vl *VaultLookup) Request(id *Identity) *Request {
	p := vl.VaultPath
	if vl.Mapped != nil {
		p = vl.Mapped.Path
	}
	return &Request{Path: p, Version: vl.Version, Identity: id}
}

// Read reads the lookup from its provider and returns the Vault response.
// Value reads are audited like every fetch (by the servers for the built-in
// store, with a client record for other providers).
func (r *VaultReader) Read(ctx context.Context, vl *VaultLookup, id *Identity) (*VaultKV, error) {
	p := vl.Resolution.Provider
	req := vl.Request(id)
	if vl.Metadata {
		mr, ok := p.(MetadataReader)
		if !ok {
			return nil, NewError(p.Name(), KindInvalid, errors.New("the provider does not support metadata reads"))
		}
		md, err := mr.ReadMetadata(ctx, req)
		if err != nil {
			return nil, err
		}
		// The version has the form of the built-in store's watch, so that
		// a watch of a deleted secret doesn't return at once.
		version := strconv.FormatUint(md.CurrentVersion, 10)
		if md.Deleted {
			version += ":deleted"
		}
		return &VaultKV{Data: kvMetadataData(md), Version: version}, nil
	}
	sec, err, _ := FetchAudited(ctx, p, req, r.Auditor)
	if err != nil {
		return nil, err
	}
	return vaultKVFromSecret(sec, vl.KVVersion), nil
}

// vaultKVFromSecret builds the KV v1 or v2 response of a secret.
func vaultKVFromSecret(sec *Secret, kvVersion int) *VaultKV {
	items := make(map[string]any, len(sec.Items))
	for k, v := range sec.Items {
		items[k] = v
	}
	if kvVersion != 2 {
		return &VaultKV{Data: items, LeaseDuration: kvV1LeaseDuration, Version: sec.Version}
	}
	md := map[string]any{
		"created_time":    vaultTime(time.Time{}),
		"custom_metadata": nil,
		"deletion_time":   "",
		"destroyed":       false,
		"version":         json.Number("0"),
	}
	if _, err := strconv.ParseUint(sec.Version, 10, 64); err == nil {
		md["version"] = json.Number(sec.Version)
	}
	if m := sec.Metadata; m != nil {
		md["created_time"] = vaultTime(m.CreatedTime)
		md["custom_metadata"] = vaultLabels(m.Labels)
		md["version"] = json.Number(strconv.FormatUint(m.Version, 10))
	}
	return &VaultKV{Data: map[string]any{"data": items, "metadata": md}, Version: sec.Version}
}

// kvMetadataData builds the data of a KV v2 metadata read. Deleted versions
// have the time of the secret's last change as their deletion time: the
// built-in store doesn't record when a version was deleted.
func kvMetadataData(md *Metadata) map[string]any {
	versions := make(map[string]any, len(md.Versions))
	for _, v := range md.Versions {
		deletion := ""
		if v.Deleted || v.Destroyed {
			deletion = vaultTime(md.UpdateTime)
		}
		versions[strconv.FormatUint(v.Version, 10)] = map[string]any{
			"created_time":  vaultTime(v.CreatedTime),
			"deletion_time": deletion,
			"destroyed":     v.Destroyed,
		}
	}
	return map[string]any{
		"cas_required":         false,
		"created_time":         vaultTime(md.CreateTime),
		"current_version":      json.Number(strconv.FormatUint(md.CurrentVersion, 10)),
		"custom_metadata":      vaultLabels(md.Labels),
		"delete_version_after": "0s",
		"max_versions":         json.Number(strconv.Itoa(md.MaxVersions)),
		"oldest_version":       json.Number(strconv.FormatUint(md.OldestVersion, 10)),
		"updated_time":         vaultTime(md.UpdateTime),
		"versions":             versions,
	}
}

// vaultTime formats a time as Vault does; an unknown time is the zero time,
// which Vault clients parse, unlike an empty string.
func vaultTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func vaultLabels(labels map[string]string) any {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]any, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return out
}

// VaultTemplateSecret is the value of the Vault "secret" template function
// served by the built-in store: the fields of consul-template's Vault secret
// that templates read (.Data, .LeaseDuration, ...).
type VaultTemplateSecret struct {
	RequestID     string
	LeaseID       string
	LeaseDuration int
	Renewable     bool
	Data          map[string]interface{}
	Warnings      []string
}

// vaultRefKey is the key of the template entries of Vault-style calls.
func vaultRefKey(ref string) string { return "\x00vault:" + ref }

// VaultPolicyWarning returns the warning of a task vault block served by the
// built-in store whose Vault policies have no counterpart: the store checks
// access with the task's workload identity, so the policies are ignored, and
// a task without a secret_access grant only reads what policies bound to its
// job allow. Empty if the task has a grant or the block names no policies.
func VaultPolicyWarning(job *structs.Job, group, task string, policies []string) string {
	if len(policies) == 0 || len(structs.SecretAccessPatterns(job, group, task)) > 0 {
		return ""
	}
	sorted := append([]string(nil), policies...)
	sort.Strings(sorted)
	return fmt.Sprintf("Vault policies %s are not used by the built-in secret store; the task has no "+
		"secret_access grant, so it reads only what ACL policies bound to its job allow",
		strings.Join(sorted, ", "))
}

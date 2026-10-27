// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secrets

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/nomad/structs"
)

// Vault-style lookups served by the built-in store (norca-doc content/docs/design/secrets-plan.md,
// "Vault compatibility"): the Vault "secret" template function and the reads
// through the client's Vault-compatible endpoint name Vault paths such as
// "kv/data/app/db". When they resolve to the built-in store, the client's
// vault_kv rules map them onto secrets of the store and select the KV
// response shape.

// VaultPath is a Vault path mapped onto the built-in store.
type VaultPath struct {
	// Path is the path of the secret in the built-in store.
	Path string

	// KVVersion is the KV engine version of the response shape (1 or 2).
	KVVersion int

	// Metadata is set for a KV v2 metadata read ("<mount>/metadata/<p>").
	Metadata bool
}

// VaultMapper maps Vault KV paths onto the built-in store with the client's
// vault_kv rules.
type VaultMapper struct {
	// rules are sorted by the length of their prefix, longest first.
	rules []*vaultRule
}

type vaultRule struct {
	prefix   string
	path     string
	version  int
	metadata bool
}

// NewVaultMapper returns the mapper of the rules; no rules are
// config.DefaultVaultKVRules.
func NewVaultMapper(rules []*config.VaultKVRule) *VaultMapper {
	if len(rules) == 0 {
		rules = config.DefaultVaultKVRules()
	}
	m := &VaultMapper{}
	for _, r := range rules {
		if r == nil {
			continue
		}
		m.rules = append(m.rules, &vaultRule{prefix: r.Prefix, path: r.Path, version: r.Version()})
		if mp := r.MetadataPrefix(); mp != "" {
			m.rules = append(m.rules, &vaultRule{prefix: mp, path: r.Path, version: 2, metadata: true})
		}
	}
	sort.SliceStable(m.rules, func(i, j int) bool {
		return len(m.rules[i].prefix) > len(m.rules[j].prefix)
	})
	return m
}

// dynamicVaultSegments are the second path segments of the Vault engines
// that create credentials on demand (database/creds/<role>,
// aws/creds/<role>, aws/sts/<role>, nomad/creds/<role>, ...).
var dynamicVaultSegments = map[string]bool{"creds": true, "sts": true, "static-creds": true}

// vaultDynamicMessage ends the error of a Vault path of a dynamic secrets
// engine served by the built-in store.
const vaultDynamicMessage = "requires a Vault/OpenBao provider or the built-in dynamic engine (not available yet)"

// Map maps a Vault path onto the built-in store. Paths that match no rule
// fail: paths of dynamic engines with a message that they need a Vault or
// OpenBao provider or the built-in dynamic engine, any other path as an
// engine the built-in store does not support. The errors never fall back.
func (m *VaultMapper) Map(vaultPath string) (*VaultPath, error) {
	p := strings.TrimPrefix(vaultPath, "/")
	for _, r := range m.rules {
		rest, ok := strings.CutPrefix(p, r.prefix)
		if !ok {
			continue
		}
		rest = strings.TrimSuffix(rest, "/")
		if rest == "" {
			return nil, NewError(structs.SecretProviderNorca, KindInvalid,
				fmt.Errorf("Vault path %q names no secret below the mount %q", vaultPath, r.prefix))
		}
		out := &VaultPath{Path: r.path + rest, KVVersion: r.version, Metadata: r.metadata}
		if err := structs.ValidateSecretPath(out.Path); err != nil {
			return nil, NewError(structs.SecretProviderNorca, KindInvalid,
				fmt.Errorf("Vault path %q maps to an invalid secret path: %v", vaultPath, err))
		}
		return out, nil
	}
	if segs := strings.Split(p, "/"); len(segs) >= 3 && dynamicVaultSegments[segs[1]] {
		return nil, NewError(structs.SecretProviderNorca, KindInvalid, &UnsupportedError{
			msg: fmt.Sprintf("Vault path %q of a dynamic secrets engine %s", vaultPath, vaultDynamicMessage)})
	}
	return nil, NewError(structs.SecretProviderNorca, KindInvalid, &UnsupportedError{
		msg: fmt.Sprintf("Vault path %q is not supported by the built-in store: only KV v1/v2 reads of the "+
			"mounts of the vault_kv rules are (%s); other secrets engines require a Vault/OpenBao provider",
			vaultPath, strings.Join(m.prefixes(), ", "))})
}

// KVVersion returns the KV version of the rule that matches the Vault path,
// 1 if none does; for the responses of providers other than the built-in
// store, which read the path as it is.
func (m *VaultMapper) KVVersion(vaultPath string) int {
	p := strings.TrimPrefix(vaultPath, "/")
	for _, r := range m.rules {
		if strings.HasPrefix(p, r.prefix) {
			return r.version
		}
	}
	return 1
}

func (m *VaultMapper) prefixes() []string {
	out := make([]string, 0, len(m.rules))
	for _, r := range m.rules {
		out = append(out, r.prefix)
	}
	sort.Strings(out)
	return out
}

// UnsupportedError is the error of a Vault path the built-in store does not
// serve (another secrets engine, a dynamic engine).
type UnsupportedError struct{ msg string }

func (e *UnsupportedError) Error() string { return e.msg }

// IsUnsupported reports whether err is the error of a Vault path the
// built-in store does not serve.
func IsUnsupported(err error) bool {
	var u *UnsupportedError
	return errors.As(err, &u)
}

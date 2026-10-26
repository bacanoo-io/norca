// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"errors"

	"github.com/hashicorp/nomad/nomad/structs"
)

// Vault compatibility on the servers (docs/design/secrets-plan.md, "Vault
// compatibility"): when the servers have no Vault integration, jobs with
// vault blocks are accepted (instead of failing with "Vault not enabled")
// and placed on the clients that serve vault blocks from the built-in secret
// store; their Vault-style lookups resolve on those clients like every
// lookup, with the built-in store as the fallback. The agent enables it by
// default (server { secrets { vault_fallback } }); with a Vault integration
// nothing changes.

// attrVaultCompat is the node attribute of the clients serving vault blocks
// from the built-in secret store (client/config.AttrVaultCompat).
const attrVaultCompat = `${attr.norca.secrets.vault_compat}`

// vaultCompatConstraint is the implicit constraint of the task groups with
// vault blocks when the servers have no Vault integration.
var vaultCompatConstraint = &structs.Constraint{
	LTarget: attrVaultCompat,
	RTarget: "true",
	Operand: "=",
}

// errVaultFallback is the registration warning of a job with vault blocks
// served by the built-in secret store.
var errVaultFallback = errors.New("Vault is not enabled: the vault blocks and Vault secret template " +
	"calls of the job are served by the built-in secret store of the clients (access is checked with the " +
	"workload identities, not with Vault policies)")

// vaultFallbackActive reports whether the servers serve vault blocks from
// the built-in secret store: no Vault integration and the fallback enabled.
func (s *Server) vaultFallbackActive() bool {
	return !s.config.VaultConfig.IsEnabled() && s.config.SecretsVaultFallback
}

// vaultFallbackEnabled is vaultFallbackActive once every server of the
// region runs Norca.
func (s *Server) vaultFallbackEnabled() bool {
	return s.vaultFallbackActive() && ServersMeetMinimumNorcaVersion(s.Members(), s.Region(), false)
}

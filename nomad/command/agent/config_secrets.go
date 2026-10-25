// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"time"

	"github.com/hashicorp/nomad/secretstore"
)

// SecretsConfig is the server's secrets block: the server side settings of
// the built-in secret store.
//
//	secrets {
//	  audit_file             = "/var/log/norca/secrets-audit.log"
//	  audit_rotate_bytes     = 104857600
//	  audit_rotate_duration  = "24h"
//	  audit_rotate_max_files = 30
//	}
//
// The seal of the store is configured in the keyring block.
type SecretsConfig struct {
	// AuditFile is the append-only audit file of this server: every value
	// read and every write of a secret executed by this server (the leader)
	// is recorded, never a value. Empty disables the file; the SecretAccess
	// event topic is always published.
	AuditFile string `hcl:"audit_file"`

	// AuditRotateBytes rotates the audit file before it grows beyond this
	// size; 0 disables size based rotation.
	AuditRotateBytes int `hcl:"audit_rotate_bytes"`

	// AuditRotateDuration rotates the audit file once it is older than this
	// (for example "24h"); empty disables time based rotation.
	AuditRotateDuration string `hcl:"audit_rotate_duration"`

	// AuditRotateMaxFiles is the number of rotated audit files kept; 0 keeps
	// all of them.
	AuditRotateMaxFiles int `hcl:"audit_rotate_max_files"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// Copy returns a copy of the block.
func (c *SecretsConfig) Copy() *SecretsConfig {
	if c == nil {
		return nil
	}
	out := *c
	out.ExtraKeysHCL = nil
	return &out
}

// Merge merges two blocks; the non-zero values of b win.
func (c *SecretsConfig) Merge(b *SecretsConfig) *SecretsConfig {
	if c == nil {
		return b.Copy()
	}
	result := c.Copy()
	if b == nil {
		return result
	}
	if b.AuditFile != "" {
		result.AuditFile = b.AuditFile
	}
	if b.AuditRotateBytes != 0 {
		result.AuditRotateBytes = b.AuditRotateBytes
	}
	if b.AuditRotateDuration != "" {
		result.AuditRotateDuration = b.AuditRotateDuration
	}
	if b.AuditRotateMaxFiles != 0 {
		result.AuditRotateMaxFiles = b.AuditRotateMaxFiles
	}
	return result
}

// toAuditFileConfig converts the block to the audit file configuration of
// the server, nil if no audit file is configured.
func (c *SecretsConfig) toAuditFileConfig() (*secretstore.AuditFileConfig, error) {
	if c == nil || c.AuditFile == "" {
		return nil, nil
	}
	if c.AuditRotateBytes < 0 {
		return nil, fmt.Errorf("secrets.audit_rotate_bytes must not be negative")
	}
	if c.AuditRotateMaxFiles < 0 {
		return nil, fmt.Errorf("secrets.audit_rotate_max_files must not be negative")
	}
	out := &secretstore.AuditFileConfig{
		Path:           c.AuditFile,
		RotateBytes:    int64(c.AuditRotateBytes),
		RotateMaxFiles: c.AuditRotateMaxFiles,
	}
	if c.AuditRotateDuration != "" {
		d, err := time.ParseDuration(c.AuditRotateDuration)
		if err != nil {
			return nil, fmt.Errorf("secrets.audit_rotate_duration: %w", err)
		}
		if d < 0 {
			return nil, fmt.Errorf("secrets.audit_rotate_duration must not be negative")
		}
		out.RotateDuration = d
	}
	return out, nil
}

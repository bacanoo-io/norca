// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"errors"
	"fmt"
	"slices"
)

// Keyring seal types of the configuration (see nomad/seal).
const (
	KeyringSealAEAD   = "aead"
	KeyringSealShares = "shares"

	// DefaultKeyringSealShares and DefaultKeyringSealThreshold are the
	// Shamir parameters of a shares seal without explicit values.
	DefaultKeyringSealShares    = 5
	DefaultKeyringSealThreshold = 3
)

// plannedKeyringSeals are the seal types of the design that are not
// implemented yet; they plug into the seal interface of nomad/seal.
var plannedKeyringSeals = []string{"awskms", "gcpckms", "scwkms", "transit"}

// KeyringConfig is the keyring block of the server configuration:
//
//	keyring {
//	  seal "shares" {
//	    threshold = 3
//	    shares    = 5
//	  }
//	}
//
// Without a keyring block the server uses the aead seal (the key-encrypting
// key is stored next to the key, as in Nomad), for development and
// migration only.
type KeyringConfig struct {
	// Seals are the seal blocks. The data model allows a second seal (keys
	// wrapped by both, either unwraps them); only one seal can be configured
	// until a second seal type that makes sense next to shares (a KMS)
	// exists.
	Seals []*KeyringSealConfig `hcl:"seal"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// KeyringSealConfig is one seal block.
type KeyringSealConfig struct {
	// Type is the label of the block: aead or shares.
	Type string `hcl:",key"`

	// Threshold and Shares are the Shamir parameters of the shares seal:
	// the number of shares generated at the first rewrap to shares, and how
	// many of them unseal a server.
	Threshold int `hcl:"threshold"`
	Shares    int `hcl:"shares"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// Copy returns a deep copy.
func (c *KeyringConfig) Copy() *KeyringConfig {
	if c == nil {
		return nil
	}
	nc := &KeyringConfig{Seals: make([]*KeyringSealConfig, 0, len(c.Seals))}
	for _, s := range c.Seals {
		ns := *s
		ns.ExtraKeysHCL = nil
		nc.Seals = append(nc.Seals, &ns)
	}
	return nc
}

// Merge returns c with o applied: a keyring block replaces the seals of an
// earlier one.
func (c *KeyringConfig) Merge(o *KeyringConfig) *KeyringConfig {
	if o == nil {
		return c.Copy()
	}
	return o.Copy()
}

// Seal returns the configured seal with defaults applied, or the aead seal
// if none is configured.
func (c *KeyringConfig) Seal() *KeyringSealConfig {
	if c == nil || len(c.Seals) == 0 {
		return &KeyringSealConfig{Type: KeyringSealAEAD}
	}
	s := *c.Seals[0]
	s.ExtraKeysHCL = nil
	if s.Type == KeyringSealShares {
		if s.Shares == 0 {
			s.Shares = DefaultKeyringSealShares
		}
		if s.Threshold == 0 {
			s.Threshold = min(DefaultKeyringSealThreshold, s.Shares)
		}
	}
	return &s
}

// Validate checks the keyring configuration.
func (c *KeyringConfig) Validate() error {
	if c == nil {
		return nil
	}
	switch {
	case len(c.Seals) > 2:
		return errors.New("at most two seals can be configured")
	case len(c.Seals) == 2:
		return errors.New("a second seal is not supported yet: configure one seal")
	}
	for _, s := range c.Seals {
		switch s.Type {
		case KeyringSealAEAD:
			if s.Threshold != 0 || s.Shares != 0 {
				return errors.New(`seal "aead" has no threshold or shares`)
			}
		case KeyringSealShares:
			shares, threshold := s.Shares, s.Threshold
			if shares == 0 {
				shares = DefaultKeyringSealShares
			}
			if threshold == 0 {
				threshold = min(DefaultKeyringSealThreshold, shares)
			}
			if err := ValidateSealShares(shares, threshold); err != nil {
				return fmt.Errorf(`seal "shares": %w`, err)
			}
		case "":
			return errors.New("seal type is required")
		default:
			if slices.Contains(plannedKeyringSeals, s.Type) {
				return fmt.Errorf("seal %q is not implemented yet: use shares (or aead for development)", s.Type)
			}
			return fmt.Errorf("unknown seal type %q", s.Type)
		}
	}
	return nil
}

// ValidateSealShares checks the Shamir parameters of a shares seal.
func ValidateSealShares(shares, threshold int) error {
	switch {
	case shares < 2 || shares > 255:
		return fmt.Errorf("shares must be between 2 and 255, got %d", shares)
	case threshold < 2 || threshold > shares:
		return fmt.Errorf("threshold must be between 2 and shares (%d), got %d", shares, threshold)
	}
	return nil
}

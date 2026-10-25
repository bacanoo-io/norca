// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package seal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/secretstore/ubix/export/core"
	ubixseal "github.com/hashicorp/nomad/secretstore/ubix/export/seal"
)

// CoreMasterKeyPath is the storage path where the uBixVault core keeps its
// master key wrapped by its auto-unseal seal (in Raft, as a secret store
// blob). The core treats the value as opaque; it is a [CoreEnvelope].
const CoreMasterKeyPath = "core/auto-master"

// coreAAD binds the wraps of the core's master key to their use.
var coreAAD = []byte("norca-secret-store-core-master-key")

// coreEnvelopeVersion is the version of the CoreEnvelope format.
const coreEnvelopeVersion = 1

// CoreEnvelope is the stored form of the core's master key: one wrap per
// seal, like a key of the keystore.
type CoreEnvelope struct {
	Version int                       `json:"norca_seal"`
	Wraps   []*structs.SealWrappedKey `json:"wraps"`
}

// ParseCoreEnvelope parses the stored master key of the core.
func ParseCoreEnvelope(b []byte) (*CoreEnvelope, error) {
	var env CoreEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, fmt.Errorf("seal: invalid wrapped secret store key: %w", err)
	}
	if env.Version != coreEnvelopeVersion {
		return nil, fmt.Errorf("seal: unknown wrapped secret store key version %d", env.Version)
	}
	return &env, nil
}

// Encode returns the stored form of the envelope.
func (e *CoreEnvelope) Encode() ([]byte, error) {
	e.Version = coreEnvelopeVersion
	return json.Marshal(e)
}

// Seals returns the seal types and key IDs of the wraps.
func (e *CoreEnvelope) Seals() []string {
	out := make([]string, 0, len(e.Wraps))
	for _, w := range e.Wraps {
		out = append(out, w.Seal+":"+w.KeyID)
	}
	return out
}

// Keyring is what CoreSeal needs from the server: wrapping with the seals
// that wrap new keys, and unwrapping with any available seal.
type Keyring interface {
	WrapKey(ctx context.Context, plaintext, aad []byte) ([]*structs.SealWrappedKey, error)
	UnwrapKey(ctx context.Context, wraps []*structs.SealWrappedKey, aad []byte) ([]byte, error)
}

// CoreSeal is the auto-unseal seal of the uBixVault core: it wraps the
// core's master key with the server's seals, so one unseal of the server
// unlocks the keystore and the core together. The core records it as an
// external seal (uBixVault's seal type for a KMS behind a command), so it
// never asks for Shamir shares of its own; its recovery shares and root
// token are not used by Norca.
type CoreSeal struct {
	Keyring Keyring
}

var _ ubixseal.Seal = (*CoreSeal)(nil)

// Type implements the uBixVault seal interface.
func (c *CoreSeal) Type() string { return core.SealTypeExternal }

// Wrap implements the uBixVault seal interface.
func (c *CoreSeal) Wrap(ctx context.Context, plaintext []byte) ([]byte, error) {
	wraps, err := c.Keyring.WrapKey(ctx, plaintext, coreAAD)
	if err != nil {
		return nil, err
	}
	return (&CoreEnvelope{Wraps: wraps}).Encode()
}

// Unwrap implements the uBixVault seal interface.
func (c *CoreSeal) Unwrap(ctx context.Context, wrapped []byte) ([]byte, error) {
	env, err := ParseCoreEnvelope(wrapped)
	if err != nil {
		return nil, err
	}
	return c.Keyring.UnwrapKey(ctx, env.Wraps, coreAAD)
}

// WrapCoreKey returns the stored form of the core's master key wrapped by
// the given seals (all must be available).
func WrapCoreKey(ctx context.Context, seals []Seal, key []byte) ([]byte, error) {
	wraps, err := WrapAll(ctx, seals, key, coreAAD)
	if err != nil {
		return nil, err
	}
	return (&CoreEnvelope{Wraps: wraps}).Encode()
}

// RewrapCoreKey unwraps the stored master key of the core with the keyring
// and returns it wrapped by the given seals.
func RewrapCoreKey(ctx context.Context, kr Keyring, env *CoreEnvelope, seals []Seal) ([]byte, error) {
	key, err := kr.UnwrapKey(ctx, env.Wraps, coreAAD)
	if err != nil {
		return nil, err
	}
	defer zero(key)
	if len(key) == 0 {
		return nil, errors.New("seal: empty secret store key")
	}
	return WrapCoreKey(ctx, seals, key)
}

// UnwrapCoreKeyWith unwraps a shares wrap of the core's master key with a
// candidate unseal key.
func UnwrapCoreKeyWith(key []byte, w *structs.SealWrappedKey) ([]byte, error) {
	return UnwrapWithKey(key, w, coreAAD)
}

// Rewrap returns the stored master key of the core wrapped again with the
// keyring's current seals. It returns (nil, nil) if nothing changes.
func (c *CoreSeal) Rewrap(ctx context.Context, wrapped []byte, same func(*CoreEnvelope) bool) ([]byte, error) {
	env, err := ParseCoreEnvelope(wrapped)
	if err != nil {
		return nil, err
	}
	if same != nil && same(env) {
		return nil, nil
	}
	key, err := c.Keyring.UnwrapKey(ctx, env.Wraps, coreAAD)
	if err != nil {
		return nil, err
	}
	defer zero(key)
	if len(key) == 0 {
		return nil, errors.New("seal: empty secret store key")
	}
	return c.Wrap(ctx, key)
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Keyring seal types (server { keyring { seal "<type>" { ... } } }). The seal
// protects the root keys of the keyring in the keystore of every server and
// the root key of the built-in secret store core. See nomad/seal.
const (
	// SealTypeAEAD is today's behaviour: every key is wrapped with a random
	// key-encrypting key stored next to it. For development and migration
	// only.
	SealTypeAEAD = "aead"

	// SealTypeShares wraps the keys with a random unseal key that is split
	// into Shamir shares held by operators; a restarted server stays sealed
	// until a threshold of shares is entered.
	SealTypeShares = "shares"
)

const errSealed = "server is sealed"

var (
	// ErrSealed is returned by every operation that needs the keys of the
	// keyring (Variables, workload identity signing, mTLS issuance, the
	// keyring of backups) while the server is sealed.
	ErrSealed = errors.New(errSealed)

	// ErrSecretStoreSealed is returned while the core of the built-in secret
	// store is sealed or not initialized. It wraps ErrSealed.
	ErrSecretStoreSealed = fmt.Errorf("secret store: %w", ErrSealed)
)

// IsErrSealed returns whether an error, possibly received over RPC as a
// string, is a sealed error.
func IsErrSealed(err error) bool {
	return err != nil && (errors.Is(err, ErrSealed) || strings.Contains(err.Error(), errSealed))
}

// SealWrappedKey is a key wrapped by one seal. A key that is protected by two
// seals (the optional second seal) has one SealWrappedKey per seal, and
// either of them unwraps it.
type SealWrappedKey struct {
	// Seal is the seal type (SealTypeAEAD, SealTypeShares, ...).
	Seal string

	// KeyID identifies the key of the seal that wrapped the key: the ID of
	// the unseal key of a shares seal, the key ID of a KMS. Empty for aead.
	KeyID string `json:",omitempty"`

	// Ciphertext is the wrapped key (nonce and AES-256-GCM ciphertext for
	// aead and shares).
	Ciphertext []byte

	// KEK is the key-encrypting key of an aead wrap, stored next to the key
	// it protects. It is empty for every other seal.
	KEK []byte `json:",omitempty"`
}

// Copy returns a deep copy.
func (w *SealWrappedKey) Copy() *SealWrappedKey {
	if w == nil {
		return nil
	}
	nw := *w
	nw.Ciphertext = slices.Clone(w.Ciphertext)
	nw.KEK = slices.Clone(w.KEK)
	return &nw
}

// SealInfo describes one seal of the cluster. It never contains a key.
type SealInfo struct {
	// Type is the seal type.
	Type string

	// KeyID identifies the key of the seal (see SealWrappedKey.KeyID).
	KeyID string

	// Threshold and Shares are the Shamir parameters of a shares seal.
	Threshold int
	Shares    int

	// KeyCheck lets a server verify an unseal key reconstructed from shares:
	// the AES-256-GCM encryption of a constant with the unseal key.
	KeyCheck []byte
}

// Copy returns a deep copy.
func (s *SealInfo) Copy() *SealInfo {
	if s == nil {
		return nil
	}
	ns := *s
	ns.KeyCheck = slices.Clone(s.KeyCheck)
	return &ns
}

// Stub returns a copy without the key check, for API responses.
func (s *SealInfo) Stub() *SealInfo {
	if s == nil {
		return nil
	}
	ns := *s
	ns.KeyCheck = nil
	return &ns
}

// SealConfig is the keyring seal configuration of a region, a singleton in
// Raft. It records which seals wrap the keys (Seals) and a seal migration in
// progress (Rewrap). Without a SealConfig in the state the region uses the
// aead seal, as Nomad did.
type SealConfig struct {
	// Seals are the seals that wrap every key of the region: one, or two
	// with the optional second seal. Empty while a new region with a shares
	// seal waits for its first rewrap, which generates the shares.
	Seals []*SealInfo

	// Rewrap is the seal migration in progress, or nil.
	Rewrap *SealRewrap

	// LastRewrapID is the ID of the last completed rewrap.
	LastRewrapID string

	CreateIndex uint64
	ModifyIndex uint64
}

// Copy returns a deep copy.
func (c *SealConfig) Copy() *SealConfig {
	if c == nil {
		return nil
	}
	nc := *c
	nc.Seals = copySealInfos(c.Seals)
	nc.Rewrap = c.Rewrap.Copy()
	return &nc
}

// Stub returns a copy without key checks, for API responses.
func (c *SealConfig) Stub() *SealConfig {
	if c == nil {
		return nil
	}
	nc := c.Copy()
	for i, s := range nc.Seals {
		nc.Seals[i] = s.Stub()
	}
	if nc.Rewrap != nil {
		for i, s := range nc.Rewrap.To {
			nc.Rewrap.To[i] = s.Stub()
		}
	}
	return nc
}

// SealTypes returns the types of the seals.
func (c *SealConfig) SealTypes() []string {
	if c == nil {
		return nil
	}
	return SealInfoTypes(c.Seals)
}

// SealInfoTypes returns the types of a list of seals.
func SealInfoTypes(seals []*SealInfo) []string {
	out := make([]string, 0, len(seals))
	for _, s := range seals {
		out = append(out, s.Type)
	}
	return out
}

func copySealInfos(in []*SealInfo) []*SealInfo {
	if in == nil {
		return nil
	}
	out := make([]*SealInfo, len(in))
	for i, s := range in {
		out[i] = s.Copy()
	}
	return out
}

// SealRewrap is a seal migration in progress: every server adds wraps of
// the To seals to its keys, keeping the wraps of the current seals, and
// reports when it is done. When every server of the region is done, the
// leader makes To the region's seals and the servers drop the other wraps.
// Until then both the old and the new seals unwrap the keys.
type SealRewrap struct {
	// ID identifies the rewrap.
	ID string

	// To are the seals the keys are moved to.
	To []*SealInfo

	// Done are the Raft IDs of the servers that have wrapped their keys
	// with the To seals.
	Done []string

	// StartTime is the start of the rewrap (Unix nanoseconds).
	StartTime int64
}

// Copy returns a deep copy.
func (r *SealRewrap) Copy() *SealRewrap {
	if r == nil {
		return nil
	}
	nr := *r
	nr.To = copySealInfos(r.To)
	nr.Done = slices.Clone(r.Done)
	return &nr
}

// SealConfigUpsertRequest writes the seal configuration of the region. It
// is applied through Raft by the leader only (SealConfigUpsertRequestType).
type SealConfigUpsertRequest struct {
	Config *SealConfig
	WriteRequest
}

// SealRewrapDoneRequest reports that a server has wrapped its keys with the
// seals of a rewrap. Servers send it to the leader (Operator.SealRewrapDone),
// which applies it through Raft (SealRewrapDoneRequestType).
type SealRewrapDoneRequest struct {
	RewrapID string
	ServerID string
	WriteRequest
}

// SealStatusRequest asks for the seal status of one server (ServerID, a
// server name or ID) or of every server of the region (ServerID empty).
type SealStatusRequest struct {
	ServerID string
	QueryOptions
}

// SealStatusResponse is the seal status of the region.
type SealStatusResponse struct {
	// Config is the seal configuration of the region, without key checks;
	// nil if the region uses the implicit aead seal.
	Config *SealConfig

	Servers []*ServerSealStatus
	QueryMeta
}

// ServerSealStatus is the seal status of one server.
type ServerSealStatus struct {
	ID      string
	Name    string
	Address string
	Leader  bool

	// Sealed is true while the server can't unwrap its keys: it serves
	// everything that needs no decryption and rejects the rest.
	Sealed bool

	// Initialized is false in a new region with a shares seal before its
	// first rewrap generated the shares.
	Initialized bool

	// Seals are the seal types of the region as this server sees them, and
	// ConfiguredSeal the seal of its agent configuration.
	Seals          []string
	ConfiguredSeal string

	// Threshold, Shares and Progress describe the unseal of a shares seal.
	Threshold int
	Shares    int
	Progress  int

	// KeyIDs are the IDs of the seal keys the server holds.
	KeyIDs []string

	// The state of the built-in secret store core on this server.
	SecretCoreInitialized bool
	SecretCoreSealed      bool
	SecretCoreActive      bool

	// Error is set if the status of the server could not be read.
	Error string
}

// SealUnsealRequest enters one unseal share on a server (ServerID, a server
// name or ID). Reset discards the shares entered so far.
type SealUnsealRequest struct {
	ServerID string

	// Key is one unseal share, base64 encoded.
	Key string

	Reset bool
	WriteRequest
}

// SealUnsealResponse is the seal status of the server after the share.
type SealUnsealResponse struct {
	Status *ServerSealStatus
	WriteMeta
}

// KeyringRewrapRequest moves the keys of the region to another seal.
type KeyringRewrapRequest struct {
	// To is the seal type to move to.
	To string

	// Shares and Threshold configure the shares of a shares seal; zero uses
	// the leader's configuration or the defaults (5 shares, threshold 3).
	Shares    int
	Threshold int
	WriteRequest
}

// KeyringRewrapResponse returns the started rewrap. Shares are the unseal
// shares of a new shares seal, base64 encoded; they are returned once and
// stored nowhere.
type KeyringRewrapResponse struct {
	RewrapID string
	Shares   []string
	Config   *SealConfig
	WriteMeta
}

// SealRewrapKeyRequest asks a server for the key of a seal of a rewrap in
// progress. Only servers may send it.
type SealRewrapKeyRequest struct {
	RewrapID string
	KeyID    string
	QueryOptions
}

// SealRewrapKeyResponse carries the key of a seal.
type SealRewrapKeyResponse struct {
	Key []byte
	QueryMeta
}

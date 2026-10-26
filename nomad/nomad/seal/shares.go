// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package seal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/hashicorp/nomad/secretstore/ubix/export/shamir"
)

// ShareSize is the size of one unseal share: a byte per key byte plus the
// x-coordinate (uBixVault's shamir format).
const ShareSize = KeySize + 1

var (
	// ErrInvalidShare is returned for a share of the wrong size.
	ErrInvalidShare = errors.New("seal: invalid unseal share")

	// ErrUnsealFailed is returned when the shares entered do not
	// reconstruct the unseal key. The shares entered so far are discarded.
	ErrUnsealFailed = errors.New("seal: unseal failed: the shares did not reconstruct the unseal key; enter the shares again")
)

// keyCheckPlaintext is encrypted with an unseal key into its key check.
var keyCheckPlaintext = []byte("norca seal key check v1")

func keyCheckAAD(keyID string) []byte { return []byte("norca-seal-key-check:" + keyID) }

// Generate creates a new unseal key for a shares seal and splits it into
// shares, any threshold of which reconstruct it. The caller hands the shares
// to the operators once and keeps the key only in memory.
func Generate(shares, threshold int) (*structs.SealInfo, []byte, [][]byte, error) {
	if err := config.ValidateSealShares(shares, threshold); err != nil {
		return nil, nil, nil, err
	}
	key, err := RandomKey()
	if err != nil {
		return nil, nil, nil, err
	}
	parts, err := shamir.Split(key, shares, threshold)
	if err != nil {
		zero(key)
		return nil, nil, nil, fmt.Errorf("seal: failed to split the unseal key: %w", err)
	}
	info := &structs.SealInfo{
		Type:      structs.SealTypeShares,
		KeyID:     uuid.Generate(),
		Threshold: threshold,
		Shares:    shares,
	}
	info.KeyCheck, err = gcmSeal(key, keyCheckPlaintext, keyCheckAAD(info.KeyID))
	if err != nil {
		zero(key)
		return nil, nil, nil, err
	}
	return info, key, parts, nil
}

// CheckKey reports whether key is the unseal key of a shares seal, using the
// key check of its info.
func CheckKey(info *structs.SealInfo, key []byte) bool {
	if info == nil || len(info.KeyCheck) == 0 {
		return false
	}
	plain, err := gcmOpen(key, info.KeyCheck, keyCheckAAD(info.KeyID))
	return err == nil && bytes.Equal(plain, keyCheckPlaintext)
}

// Shares is the shares seal of a server. It is unavailable (sealed) until
// it holds the unseal key: entered as shares with Unseal, or set with
// SetKey during a rewrap.
type Shares struct {
	mu sync.Mutex

	// info describes the unseal key: its ID, the Shamir parameters and the
	// key check. It comes from the region's seal configuration.
	info *structs.SealInfo

	// key is the unseal key, nil while sealed.
	key []byte

	// progress holds the distinct shares entered toward the next unseal.
	progress [][]byte
}

var _ Seal = (*Shares)(nil)

// NewShares returns a sealed shares seal.
func NewShares() *Shares { return &Shares{} }

// Type implements Seal.
func (s *Shares) Type() string { return structs.SealTypeShares }

// KeyID implements Seal: the ID of the unseal key held, or of the region's
// unseal key while sealed.
func (s *Shares) KeyID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.info == nil {
		return ""
	}
	return s.info.KeyID
}

// Available implements Seal.
func (s *Shares) Available() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.key != nil
}

// Info returns a copy of the description of the unseal key, or nil.
func (s *Shares) Info() *structs.SealInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info.Copy()
}

// SetInfo records the region's description of the unseal key. While the
// seal holds a key, only the description of that key is accepted.
func (s *Shares) SetInfo(info *structs.SealInfo) {
	if info == nil || info.Type != structs.SealTypeShares {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key != nil && s.info != nil && s.info.KeyID != info.KeyID {
		return
	}
	if s.info == nil || s.info.KeyID != info.KeyID {
		s.resetProgressLocked()
	}
	s.info = info.Copy()
}

// SetKey makes the seal available with a known unseal key. The key must
// match the key check of info, if info has one.
func (s *Shares) SetKey(info *structs.SealInfo, key []byte) error {
	if info == nil || info.Type != structs.SealTypeShares || info.KeyID == "" {
		return errors.New("seal: invalid shares seal description")
	}
	if len(key) != KeySize {
		return fmt.Errorf("seal: keys must be %d bytes", KeySize)
	}
	if len(info.KeyCheck) > 0 && !CheckKey(info, key) {
		return ErrUnsealFailed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key != nil {
		zero(s.key)
	}
	s.info = info.Copy()
	s.key = slices.Clone(key)
	s.resetProgressLocked()
	return nil
}

// Key returns the description and a copy of the unseal key, or nil if the
// seal is sealed. It never leaves the server in usable form: during a
// rewrap the leader wraps it with the region's current seals.
func (s *Shares) Key() (*structs.SealInfo, []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key == nil {
		return nil, nil
	}
	return s.info.Copy(), slices.Clone(s.key)
}

// Clear drops the unseal key: the seal is sealed again.
func (s *Shares) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key != nil {
		zero(s.key)
		s.key = nil
	}
	s.resetProgressLocked()
}

// Progress returns the number of distinct shares entered toward the next
// unseal.
func (s *Shares) Progress() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.progress)
}

// ResetProgress discards the shares entered so far.
func (s *Shares) ResetProgress() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resetProgressLocked()
}

func (s *Shares) resetProgressLocked() {
	for _, p := range s.progress {
		zero(p)
	}
	s.progress = nil
}

// Unseal enters one share. Once threshold distinct shares are entered they
// are combined into a key, which verify must accept (verify may be nil if
// the seal's info has a key check, which is then used). It returns the
// number of shares entered and whether the seal is now unsealed; a share
// entered while unsealed is ignored. A wrong combination discards the
// shares and returns ErrUnsealFailed.
func (s *Shares) Unseal(share []byte, threshold int, verify func(key []byte) error) (int, bool, error) {
	if len(share) != ShareSize {
		return 0, false, ErrInvalidShare
	}
	if threshold < 2 {
		return 0, false, errors.New("seal: the threshold of the shares is unknown")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key != nil {
		return 0, true, nil
	}
	if !slices.ContainsFunc(s.progress, func(p []byte) bool { return bytes.Equal(p, share) }) {
		s.progress = append(s.progress, slices.Clone(share))
	}
	if len(s.progress) < threshold {
		return len(s.progress), false, nil
	}

	key, err := shamir.Combine(s.progress)
	s.resetProgressLocked()
	if err != nil {
		return 0, false, fmt.Errorf("%w: %v", ErrUnsealFailed, err)
	}
	switch {
	case verify != nil:
		err = verify(key)
	case s.info != nil && CheckKey(s.info, key):
	default:
		err = ErrUnsealFailed
	}
	if err != nil {
		zero(key)
		return 0, false, err
	}
	s.key = key
	return 0, true, nil
}

// Wrap implements Seal.
func (s *Shares) Wrap(_ context.Context, plaintext, aad []byte) (*structs.SealWrappedKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key == nil {
		return nil, fmt.Errorf("%w: the shares seal is not unsealed", structs.ErrSealed)
	}
	ct, err := gcmSeal(s.key, plaintext, aad)
	if err != nil {
		return nil, err
	}
	return &structs.SealWrappedKey{Seal: structs.SealTypeShares, KeyID: s.info.KeyID, Ciphertext: ct}, nil
}

// Unwrap implements Seal.
func (s *Shares) Unwrap(_ context.Context, w *structs.SealWrappedKey, aad []byte) ([]byte, error) {
	if w == nil || w.Seal != structs.SealTypeShares {
		return nil, errors.New("seal: not a shares wrap")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key == nil {
		return nil, fmt.Errorf("%w: the shares seal is not unsealed", structs.ErrSealed)
	}
	if w.KeyID != s.info.KeyID {
		return nil, ErrKeyMismatch
	}
	return gcmOpen(s.key, w.Ciphertext, aad)
}

// UnwrapWithKey unwraps a shares wrap with a candidate unseal key. It is
// used to verify a key reconstructed from shares against the keystore when
// the region's key check is not known.
func UnwrapWithKey(key []byte, w *structs.SealWrappedKey, aad []byte) ([]byte, error) {
	if w == nil || w.Seal != structs.SealTypeShares {
		return nil, errors.New("seal: not a shares wrap")
	}
	return gcmOpen(key, w.Ciphertext, aad)
}

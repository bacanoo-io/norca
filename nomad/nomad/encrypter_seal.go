// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/nomad/nomad/seal"
	"github.com/hashicorp/nomad/nomad/structs"
)

// The keystore and the seal (see nomad/seal): every key file holds the root
// key encrypted with a key-encrypting key (KEK), and the KEK wrapped by each
// seal of the region. A key whose wraps can't be unwrapped at start (the
// server is sealed) is kept in sealedKeys until the server is unsealed.

// keystoreAAD binds the wrap of a KEK to its root key.
func keystoreAAD(keyID string) []byte { return []byte("norca-keystore:" + keyID) }

// sealMgr returns the seal of the server, or nil for a standalone keyring
// (tests, the keyring of a backup), which uses the aead seal.
func (e *Encrypter) sealMgr() *sealManager {
	if e.srv == nil {
		return nil
	}
	return e.srv.seal
}

func (e *Encrypter) shutdownCtx() context.Context {
	if e.srv == nil || e.srv.shutdownCtx == nil {
		return context.Background()
	}
	return e.srv.shutdownCtx
}

// isSealed reports whether the server is sealed. It must not be called with
// the keyring lock held.
func (e *Encrypter) isSealed() bool {
	m := e.sealMgr()
	return m != nil && m.sealed()
}

// hasKeys reports whether the keystore holds any key, wrapped or not.
func (e *Encrypter) hasKeys() bool {
	e.lock.RLock()
	defer e.lock.RUnlock()
	return len(e.keyring) > 0 || len(e.sealedKeys) > 0
}

// keystoreSeals returns the seals key files are wrapped with.
func (e *Encrypter) keystoreSeals() ([]seal.Seal, error) {
	m := e.sealMgr()
	if m == nil {
		return []seal.Seal{seal.AEAD{}}, nil
	}
	return m.wrapSeals()
}

// sealWrapTag identifies the seal of a wrap: its type, and the seal key ID
// for seals with keys.
func sealWrapTag(sealType, keyID string) string {
	if keyID == "" {
		return sealType
	}
	return sealType + ":" + keyID
}

// sealWrapTagsOfFile returns the seal wraps of a key file, sorted.
func sealWrapTagsOfFile(w *structs.KeyEncryptionKeyWrapper) []string {
	var tags []string
	if len(w.KeyEncryptionKey) > 0 {
		tags = append(tags, structs.SealTypeAEAD)
	}
	for _, sw := range w.SealedKEKs {
		tags = append(tags, sealWrapTag(sw.Seal, sw.KeyID))
	}
	slices.Sort(tags)
	return tags
}

// sealWrapTags returns the wraps a key file gets from a set of seals.
func sealWrapTags(seals []seal.Seal) []string {
	tags := make([]string, 0, len(seals))
	for _, s := range seals {
		tags = append(tags, sealWrapTag(s.Type(), s.KeyID()))
	}
	slices.Sort(tags)
	return tags
}

func (e *Encrypter) setStoredWraps(keyID string, tags []string) {
	e.wrapsLock.Lock()
	defer e.wrapsLock.Unlock()
	if e.storedWraps == nil {
		return
	}
	e.storedWraps[keyID] = tags
}

func (e *Encrypter) storedWrapsOf(keyID string) []string {
	e.wrapsLock.Lock()
	defer e.wrapsLock.Unlock()
	return e.storedWraps[keyID]
}

// keystoreSharesKeyID returns the shares seal key ID of a key file that has
// no aead wrap, or an empty string.
func (e *Encrypter) keystoreSharesKeyID() string {
	e.wrapsLock.Lock()
	defer e.wrapsLock.Unlock()
	for _, tags := range e.storedWraps {
		if slices.Contains(tags, structs.SealTypeAEAD) {
			continue
		}
		for _, t := range tags {
			if keyID, ok := strings.CutPrefix(t, structs.SealTypeShares+":"); ok {
				return keyID
			}
		}
	}
	return ""
}

// sealedPublicKeyLocked returns the public key of a key that is still
// wrapped, or nil. The caller holds the keyring lock.
func (e *Encrypter) sealedPublicKeyLocked(keyID string) ed25519.PublicKey {
	w, ok := e.sealedKeys[keyID]
	if !ok || len(w.PublicKey) != ed25519.PublicKeySize {
		return nil
	}
	return ed25519.PublicKey(w.PublicKey)
}

// unsealKeystore unwraps the keys that were sealed, once the server is
// unsealed. Keys that still can't be unwrapped stay sealed.
func (e *Encrypter) unsealKeystore() error {
	e.lock.RLock()
	pending := make([]*structs.KeyEncryptionKeyWrapper, 0, len(e.sealedKeys))
	for _, w := range e.sealedKeys {
		pending = append(pending, w)
	}
	e.lock.RUnlock()

	var firstErr error
	for _, w := range pending {
		key, err := e.unwrapKeyFile(w)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := e.addCipher(key); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		e.lock.Lock()
		delete(e.sealedKeys, key.Meta.KeyID)
		e.lock.Unlock()
	}
	return firstErr
}

// rewrapKeystore writes every key file whose wraps differ from the seals
// keys are wrapped with now: during a rewrap it adds the wraps of the new
// seals, afterwards it drops the wraps of the seals no longer used.
func (e *Encrypter) rewrapKeystore() error {
	seals, err := e.keystoreSeals()
	if err != nil {
		return err
	}
	want := sealWrapTags(seals)

	e.lock.RLock()
	keys := make([]*structs.RootKey, 0, len(e.keyring))
	for _, ks := range e.keyring {
		keys = append(keys, ks.rootKey)
	}
	e.lock.RUnlock()

	e.storeLock.Lock()
	defer e.storeLock.Unlock()
	for _, key := range keys {
		if slices.Equal(e.storedWrapsOf(key.Meta.KeyID), want) {
			continue
		}
		if err := e.saveKeyLocked(key, seals); err != nil {
			return fmt.Errorf("failed to rewrap root key %s: %w", key.Meta.KeyID, err)
		}
	}
	return nil
}

// verifySharesKey reports whether a candidate unseal key unwraps a key of
// the keystore wrapped by the shares seal with the given key ID.
func (e *Encrypter) verifySharesKey(keyID string, key []byte) bool {
	e.lock.RLock()
	defer e.lock.RUnlock()
	for id, w := range e.sealedKeys {
		for _, sw := range w.SealedKEKs {
			if sw.Seal != structs.SealTypeShares || sw.KeyID != keyID {
				continue
			}
			if _, err := seal.UnwrapWithKey(key, sw, keystoreAAD(id)); err == nil {
				return true
			}
		}
	}
	return false
}

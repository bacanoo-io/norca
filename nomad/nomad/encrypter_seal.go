// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/nomad/helper/crypto"
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
// keys are wrapped with now. During a rewrap (add) it adds the wraps of the
// new seals. Otherwise it drops the wraps of the seals no longer used and
// adds wraps of seals other than aead only (a server that was down during a
// rewrap away from aead wraps its files once it is unsealed), but never an
// aead wrap: a seal configuration that does not match the keystore, as for
// a moment during a snapshot restore, can't weaken it.
func (e *Encrypter) rewrapKeystore(add bool) error {
	seals, err := e.keystoreSeals()
	if err != nil {
		return err
	}

	e.lock.RLock()
	keys := make([]*structs.RootKey, 0, len(e.keyring))
	for _, ks := range e.keyring {
		keys = append(keys, ks.rootKey)
	}
	e.lock.RUnlock()

	e.storeLock.Lock()
	defer e.storeLock.Unlock()
	for _, key := range keys {
		have := e.storedWrapsOf(key.Meta.KeyID)
		use := seals
		if !add {
			use = keepOrStrengthen(seals, have)
			if len(use) == 0 {
				continue
			}
		}
		if slices.Equal(have, sealWrapTags(use)) {
			continue
		}
		if err := e.saveKeyLocked(key, use); err != nil {
			return fmt.Errorf("failed to rewrap root key %s: %w", key.Meta.KeyID, err)
		}
	}
	return nil
}

// wrapWithActiveKey wraps a key with the active root key of the keyring
// (structs.SealWrapKeyring), bound to aad.
func (e *Encrypter) wrapWithActiveKey(plaintext, aad []byte) (*structs.SealWrappedKey, error) {
	if e.isSealed() {
		return nil, structs.ErrSealed
	}
	ks, err := e.activeKeySet()
	if err != nil {
		return nil, err
	}
	nonce, err := crypto.Bytes(ks.cipher.NonceSize())
	if err != nil {
		return nil, err
	}
	keyID := ks.rootKey.Meta.KeyID
	ct := ks.cipher.Seal(nonce, nonce, plaintext, append([]byte(keyID+":"), aad...))
	return &structs.SealWrappedKey{Seal: structs.SealWrapKeyring, KeyID: keyID, Ciphertext: ct}, nil
}

// unwrapWithRootKey unwraps a wrap made by wrapWithActiveKey. It returns an
// error wrapping structs.ErrSealed if this server does not hold the root
// key.
func (e *Encrypter) unwrapWithRootKey(w *structs.SealWrappedKey, aad []byte) ([]byte, error) {
	if w == nil || w.Seal != structs.SealWrapKeyring {
		return nil, fmt.Errorf("not a keyring wrap")
	}
	if e.isSealed() {
		return nil, structs.ErrSealed
	}
	e.lock.RLock()
	ks, err := e.keysetByIDLocked(w.KeyID)
	e.lock.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("%w: root key %s is not unwrapped on this server", structs.ErrSealed, w.KeyID)
	}
	n := ks.cipher.NonceSize()
	if len(w.Ciphertext) < n {
		return nil, fmt.Errorf("malformed keyring wrap")
	}
	return ks.cipher.Open(nil, w.Ciphertext[:n], w.Ciphertext[n:], append([]byte(w.KeyID+":"), aad...))
}

// keepOrStrengthen returns the seals of a key outside a rewrap: those that
// already wrap it (have, see sealWrapTag) and those other than aead.
func keepOrStrengthen(seals []seal.Seal, have []string) []seal.Seal {
	return slices.DeleteFunc(slices.Clone(seals), func(s seal.Seal) bool {
		return s.Type() == structs.SealTypeAEAD && !slices.Contains(have, sealWrapTag(s.Type(), s.KeyID()))
	})
}

// keystoreUsesSeal reports whether a key file is wrapped by a seal key.
func (e *Encrypter) keystoreUsesSeal(sealType, keyID string) bool {
	tag := sealWrapTag(sealType, keyID)
	e.wrapsLock.Lock()
	defer e.wrapsLock.Unlock()
	for _, tags := range e.storedWraps {
		if slices.Contains(tags, tag) {
			return true
		}
	}
	return false
}

// wrapKeyForTransfer returns a root key of the keyring in the form it is
// sent to another server of a region with a seal other than aead: encrypted
// with a new key-encrypting key that is wrapped by those seals only, as in
// the keystore, never in usable form.
func (e *Encrypter) wrapKeyForTransfer(keyID string) (*structs.KeyEncryptionKeyWrapper, error) {
	m := e.sealMgr()
	if m == nil {
		return nil, fmt.Errorf("%w: the keyring has no seal to wrap the key with", structs.ErrSealed)
	}
	if m.sealed() {
		return nil, structs.ErrSealed
	}
	seals, err := m.transferSeals()
	if err != nil {
		return nil, err
	}
	e.lock.RLock()
	ks, err := e.keysetByIDLocked(keyID)
	e.lock.RUnlock()
	if err != nil {
		return nil, err
	}
	return e.newKeyFile(ks.rootKey, seals)
}

// unwrapTransferredKey unwraps a root key received from another server with
// a seal of this server.
func (e *Encrypter) unwrapTransferredKey(keyID string, w *structs.KeyEncryptionKeyWrapper) (*structs.RootKey, error) {
	if w == nil || w.Meta == nil {
		return nil, fmt.Errorf("wrapped root key without metadata")
	}
	if keyID != "" && w.Meta.KeyID != keyID {
		return nil, fmt.Errorf("wrapped root key %s is not key %s", w.Meta.KeyID, keyID)
	}
	if err := w.Meta.Validate(); err != nil {
		return nil, err
	}
	return e.unwrapKeyFile(w)
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

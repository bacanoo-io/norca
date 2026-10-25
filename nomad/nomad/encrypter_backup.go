// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"crypto/subtle"
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/nomad/nomad/structs"
)

// exportKeys returns copies of all root keys of the keyring, for the
// (encrypted) keyring object of a backup.
func (e *Encrypter) exportKeys() []*structs.RootKey {
	e.lock.RLock()
	defer e.lock.RUnlock()

	keys := make([]*structs.RootKey, 0, len(e.keyring))
	for _, ks := range e.keyring {
		keys = append(keys, &structs.RootKey{
			Meta: ks.rootKey.Meta.Copy(),
			Key:  slices.Clone(ks.rootKey.Key),
		})
	}
	slices.SortFunc(keys, func(a, b *structs.RootKey) int {
		return strings.Compare(a.Meta.KeyID, b.Meta.KeyID)
	})
	return keys
}

// restoreKeys adds the root keys of a backup to the keyring and the on-disk
// keystore, alongside the existing keys, and returns the IDs of the keys
// that were added. Key material is immutable: a key that is already in the
// keyring is skipped, unless its material differs, which is an error.
func (e *Encrypter) restoreKeys(keys []*structs.RootKey) ([]string, error) {
	var added []string
	for _, key := range keys {
		if key == nil || key.Meta == nil {
			return added, fmt.Errorf("root key without metadata")
		}
		if err := key.Meta.Validate(); err != nil {
			return added, fmt.Errorf("invalid root key %s: %w", key.Meta.KeyID, err)
		}
		if len(key.Key) == 0 {
			return added, fmt.Errorf("root key %s has no key material", key.Meta.KeyID)
		}
		if existing, err := e.GetKey(key.Meta.KeyID); err == nil {
			if subtle.ConstantTimeCompare(existing, key.Key) != 1 {
				return added, fmt.Errorf("root key %s already exists with different key material", key.Meta.KeyID)
			}
			continue
		}
		restored := &structs.RootKey{Meta: key.Meta.Copy(), Key: slices.Clone(key.Key)}
		if err := e.AddKey(restored); err != nil {
			return added, fmt.Errorf("failed to add root key %s: %w", key.Meta.KeyID, err)
		}
		added = append(added, key.Meta.KeyID)
	}
	return added, nil
}

// newBackupKeyring returns a standalone keyring holding the root keys of a
// backup. It can only decrypt; it is never written to disk.
func newBackupKeyring(keys []*structs.RootKey) (*Encrypter, error) {
	e := &Encrypter{keyring: make(map[string]*keyset)}
	for _, key := range keys {
		if err := e.addCipher(key); err != nil {
			return nil, fmt.Errorf("invalid root key in backup: %w", err)
		}
	}
	return e, nil
}

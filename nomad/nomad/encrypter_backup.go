// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
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

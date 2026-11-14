// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import "fmt"

// RootKeyUses counts what in the state still needs a root key of the
// keyring: what IsRootKeyMetaInUse looks at, by kind.
type RootKeyUses struct {
	// Allocations whose workload identity the key signed and that are not
	// terminal on both the server and the client.
	Allocations int

	// Variables encrypted with the key.
	Variables int

	// MTLSRoots are the certificate authorities of the native mTLS whose
	// private key is encrypted with the key.
	MTLSRoots int
}

// InUse reports whether anything of the state needs the key.
func (u *RootKeyUses) InUse() bool {
	return u.Allocations > 0 || u.Variables > 0 || u.MTLSRoots > 0
}

// RootKeyMetaUses counts the uses of the root key keyID in the state.
func (s *StateStore) RootKeyMetaUses(keyID string) (*RootKeyUses, error) {
	txn := s.db.ReadTxn()
	uses := &RootKeyUses{}

	count := func(table, index string, args ...interface{}) (int, error) {
		iter, err := txn.Get(table, index, args...)
		if err != nil {
			return 0, fmt.Errorf("%s lookup failed: %w", table, err)
		}
		n := 0
		for iter.Next() != nil {
			n++
		}
		return n, nil
	}

	var err error
	if uses.Allocations, err = count(TableAllocs, indexSigningKey, keyID, true); err != nil {
		return nil, err
	}
	if uses.Variables, err = count(TableVariables, indexKeyID, keyID); err != nil {
		return nil, err
	}
	if uses.MTLSRoots, err = count(TableMTLSRoots, indexSigningKeyID, keyID); err != nil {
		return nil, err
	}
	return uses, nil
}

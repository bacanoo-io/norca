// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package state

import (
	"fmt"

	"github.com/bacanoo-io/norca/norca/norca/structs"
	memdb "github.com/hashicorp/go-memdb"
)

// TableCertificateAuthorities holds the issuing CAs of the built-in
// certificate issuer. It is a table of its own, so that nothing that reads
// the trust bundle of the native mTLS can pick them up.
const TableCertificateAuthorities = "certificate_authorities"

func init() {
	RegisterSchemaFactories(certificateAuthoritiesTableSchema)
}

// certificateAuthoritiesTableSchema returns the MemDB schema of the
// certificate_authorities table.
func certificateAuthoritiesTableSchema() *memdb.TableSchema {
	return &memdb.TableSchema{
		Name: TableCertificateAuthorities,
		Indexes: map[string]*memdb.IndexSchema{
			indexID: {
				Name:         indexID,
				AllowMissing: false,
				Unique:       true,
				Indexer: &memdb.StringFieldIndex{
					Field: "ID",
				},
			},
			indexSigningKeyID: {
				Name:         indexSigningKeyID,
				AllowMissing: true,
				Unique:       false,
				Indexer: &memdb.StringFieldIndex{
					Field: "SigningKeyID",
				},
			},
		},
	}
}

// CertificateAuthorities returns all issuing CAs.
func (s *StateStore) CertificateAuthorities(ws memdb.WatchSet) ([]*structs.CertificateAuthority, error) {
	txn := s.db.ReadTxn()
	iter, err := txn.Get(TableCertificateAuthorities, indexID)
	if err != nil {
		return nil, fmt.Errorf("certificate authority lookup failed: %w", err)
	}
	ws.Add(iter.WatchCh())

	var out []*structs.CertificateAuthority
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		out = append(out, raw.(*structs.CertificateAuthority))
	}
	return out, nil
}

// UpsertCertificateAuthorities inserts or updates issuing CAs and deletes
// those with the given IDs, in one transaction.
func (s *StateStore) UpsertCertificateAuthorities(msgType structs.MessageType, index uint64,
	authorities []*structs.CertificateAuthority, deleteIDs []string) error {

	txn := s.db.WriteTxnMsgT(msgType, index)
	defer txn.Abort()

	for _, id := range deleteIDs {
		existing, err := txn.First(TableCertificateAuthorities, indexID, id)
		if err != nil {
			return fmt.Errorf("certificate authority lookup failed: %w", err)
		}
		if existing == nil {
			continue
		}
		if err := txn.Delete(TableCertificateAuthorities, existing); err != nil {
			return fmt.Errorf("certificate authority deletion failed: %w", err)
		}
	}

	for _, authority := range authorities {
		if authority == nil {
			continue
		}
		existing, err := txn.First(TableCertificateAuthorities, indexID, authority.ID)
		if err != nil {
			return fmt.Errorf("certificate authority lookup failed: %w", err)
		}
		authority = authority.Copy()
		if existing != nil {
			authority.CreateIndex = existing.(*structs.CertificateAuthority).CreateIndex
		} else {
			authority.CreateIndex = index
		}
		authority.ModifyIndex = index
		if err := txn.Insert(TableCertificateAuthorities, authority); err != nil {
			return fmt.Errorf("certificate authority insert failed: %w", err)
		}
	}

	if err := txn.Insert(tableIndex, &IndexEntry{TableCertificateAuthorities, index}); err != nil {
		return fmt.Errorf("index update failed: %w", err)
	}
	return txn.Commit()
}

// CertificateAuthorityRestore is used to restore an issuing CA from a
// snapshot.
func (r *StateRestore) CertificateAuthorityRestore(authority *structs.CertificateAuthority) error {
	if err := r.txn.Insert(TableCertificateAuthorities, authority); err != nil {
		return fmt.Errorf("certificate authority insert failed: %w", err)
	}
	return nil
}

// isRootKeyInUseByCertificates returns whether a root key of the keyring
// encrypts the private key of an issuing CA.
func isRootKeyInUseByCertificates(txn ReadTxn, keyID string) (bool, error) {
	raw, err := txn.First(TableCertificateAuthorities, indexSigningKeyID, keyID)
	if err != nil {
		return false, fmt.Errorf("certificate authority lookup failed: %w", err)
	}
	return raw != nil, nil
}

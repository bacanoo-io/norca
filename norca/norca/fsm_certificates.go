// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	metrics "github.com/hashicorp/go-metrics"
	"github.com/hashicorp/go-msgpack/codec"
	"github.com/hashicorp/raft"
)

// CertificateAuthoritySnapshot is the snapshot type of the issuing CAs. It
// uses the number of the Raft message type assigned to them.
const CertificateAuthoritySnapshot SnapshotType = 117

func (n *nomadFSM) applyCertificateAuthoritiesUpsert(msgType structs.MessageType, buf []byte, index uint64) interface{} {
	defer metrics.MeasureSince([]string{"server", "fsm", "apply_certificate_authorities_upsert"}, time.Now())
	var req structs.CertificateAuthoritiesUpsertRequest
	if err := structs.Decode(buf, &req); err != nil {
		panic(fmt.Errorf("failed to decode request: %v", err))
	}

	if err := n.state.UpsertCertificateAuthorities(msgType, index, req.Authorities, req.DeleteIDs); err != nil {
		n.logger.Error("UpsertCertificateAuthorities failed", "error", err)
		return err
	}
	return nil
}

// restoreCertificateAuthoritySnapshot restores an issuing CA.
func restoreCertificateAuthoritySnapshot(restore *state.StateRestore, dec *codec.Decoder) error {
	authority := new(structs.CertificateAuthority)
	if err := dec.Decode(authority); err != nil {
		return err
	}
	return restore.CertificateAuthorityRestore(authority)
}

// persistCertificateAuthorities persists the issuing CAs with their
// encrypted keys. They are part of every Raft snapshot and therefore of
// every backup; the root keys that decrypt the keys are in the keyring
// object of the backup.
func (s *nomadSnapshot) persistCertificateAuthorities(sink raft.SnapshotSink, encoder *codec.Encoder) error {
	authorities, err := s.snap.CertificateAuthorities(nil)
	if err != nil {
		return err
	}
	for _, authority := range authorities {
		sink.Write([]byte{byte(CertificateAuthoritySnapshot)})
		if err := encoder.Encode(authority); err != nil {
			return err
		}
	}
	return nil
}

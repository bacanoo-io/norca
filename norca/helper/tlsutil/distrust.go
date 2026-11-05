// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tlsutil

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"sync"
	"time"
)

// The distrusted CAs of the incoming connections. The TLS of Norca has no
// revocation, and a CA that chains up to the cluster CA stays valid until it
// expires: a node CA whose key was stolen would let the thief join nodes
// until then. A server shuts it out by listing it in distrusted_ca_file: a
// client certificate whose chain has a certificate with the public key of a
// listed one is refused. The match is on the public key, so a CA issued again
// with the same key is refused too.
//
// The file is read again when it changes, without a restart or a reload. When
// it can't be read any more, the last list that could be read stays.

// distrustList is the list of distrusted public keys read from a PEM file.
type distrustList struct {
	path string

	mu      sync.Mutex
	modTime time.Time
	size    int64
	keys    map[[sha256.Size]byte]string // the subject of each listed certificate
}

// newDistrustList reads the file. It is an error when the file can't be read
// or has no certificate: a list that is set must be in force.
func newDistrustList(path string) (*distrustList, error) {
	d := &distrustList{path: path}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read the distrusted CA file: %v", err)
	}
	keys, err := readDistrusted(path)
	if err != nil {
		return nil, err
	}
	d.keys, d.modTime, d.size = keys, info.ModTime(), info.Size()
	return d, nil
}

// readDistrusted reads the public keys of the certificates of a PEM file.
func readDistrusted(path string) (map[[sha256.Size]byte]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read the distrusted CA file: %v", err)
	}
	keys := map[[sha256.Size]byte]string{}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse a certificate of the distrusted CA file %s: %v", path, err)
		}
		keys[sha256.Sum256(cert.RawSubjectPublicKeyInfo)] = cert.Subject.String()
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no certificate in the distrusted CA file %s", path)
	}
	return keys, nil
}

// current returns the list, read again when the file changed.
func (d *distrustList) current() map[[sha256.Size]byte]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	info, err := os.Stat(d.path)
	if err != nil || (info.ModTime().Equal(d.modTime) && info.Size() == d.size) {
		return d.keys
	}
	if keys, err := readDistrusted(d.path); err == nil {
		d.keys = keys
		d.modTime, d.size = info.ModTime(), info.Size()
	}
	return d.keys
}

// verify is the VerifyPeerCertificate of an incoming TLS configuration: it
// refuses a certificate whose verified chain has a distrusted public key.
func (d *distrustList) verify(_ [][]byte, chains [][]*x509.Certificate) error {
	keys := d.current()
	for _, chain := range chains {
		for _, cert := range chain {
			if subject, ok := keys[sha256.Sum256(cert.RawSubjectPublicKeyInfo)]; ok {
				return fmt.Errorf("the certificate chain has a distrusted CA (%s)", subject)
			}
		}
	}
	return nil
}

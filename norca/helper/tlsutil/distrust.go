// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tlsutil

import (
	"crypto/sha256"
	"crypto/tls"
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
//
// A change is told by the file (another one renamed over it), its
// modification and change times and its size, and the file is read again
// every distrustReread whatever they say (Norca, review finding HLP-10: a
// list replaced by one of the same size within the resolution of the
// modification time, or copied with its time kept, stayed unread, and a CA
// just listed was still accepted).

// distrustList is the list of distrusted public keys read from a PEM file.
type distrustList struct {
	path string

	mu      sync.Mutex
	info    os.FileInfo // the file read, for os.SameFile
	modTime time.Time
	ctime   time.Time // the change time, zero where it is not known
	size    int64
	readAt  time.Time                    // when the file was read last
	keys    map[[sha256.Size]byte]string // the subject of each listed certificate

	now func() time.Time // time.Now; set by tests
}

// distrustReread is how often the list is read again even when nothing tells
// that the file changed.
const distrustReread = 30 * time.Second

// newDistrustList reads the file. It is an error when the file can't be read
// or has no certificate: a list that is set must be in force.
func newDistrustList(path string) (*distrustList, error) {
	d := &distrustList{path: path, now: time.Now}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read the distrusted CA file: %v", err)
	}
	keys, err := readDistrusted(path)
	if err != nil {
		return nil, err
	}
	d.keys = keys
	d.noteRead(info)
	return d, nil
}

// noteRead keeps what tells a change of the file read.
func (d *distrustList) noteRead(info os.FileInfo) {
	d.info, d.modTime, d.ctime, d.size = info, info.ModTime(), changeTime(info), info.Size()
	d.readAt = d.now()
}

// changed reports whether the file may have changed since it was read.
func (d *distrustList) changed(info os.FileInfo) bool {
	return !os.SameFile(info, d.info) || !info.ModTime().Equal(d.modTime) ||
		!changeTime(info).Equal(d.ctime) || info.Size() != d.size ||
		d.now().Sub(d.readAt) >= distrustReread
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
	if err != nil || !d.changed(info) {
		return d.keys
	}
	if keys, err := readDistrusted(d.path); err == nil {
		d.keys = keys
		d.noteRead(info)
	} else {
		// Tried again at the next change, or after distrustReread.
		d.readAt = d.now()
	}
	return d.keys
}

// verify is the VerifyConnection of an incoming TLS configuration: it refuses
// a certificate whose verified chain has a distrusted public key. It is not
// the VerifyPeerCertificate, which a resumed TLS session skips: the session
// ticket of a connection made before a CA was listed would keep its holder
// in until the ticket expires. VerifyConnection runs on every handshake, with
// the verified chains of the session when it is resumed.
func (d *distrustList) verify(cs tls.ConnectionState) error {
	keys := d.current()
	for _, chain := range cs.VerifiedChains {
		for _, cert := range chain {
			if subject, ok := keys[sha256.Sum256(cert.RawSubjectPublicKeyInfo)]; ok {
				return fmt.Errorf("the certificate chain has a distrusted CA (%s)", subject)
			}
		}
	}
	return nil
}

// ConnectionVerifier is the type of tls.Config.VerifyConnection.
type ConnectionVerifier func(cs tls.ConnectionState) error

// ChainConnectionVerifiers returns a VerifyConnection that runs the checks in
// order and fails with the first error; nil checks are skipped. A user of
// IncomingTLSConfig that adds a check of its own (the name and region of an
// RPC peer, for example) must chain it with the check that is there, the
// distrusted CAs, instead of replacing it. A check on the peer belongs in
// VerifyConnection: VerifyPeerCertificate does not run when a session is
// resumed.
func ChainConnectionVerifiers(checks ...ConnectionVerifier) ConnectionVerifier {
	return func(cs tls.ConnectionState) error {
		for _, check := range checks {
			if check == nil {
				continue
			}
			if err := check(cs); err != nil {
				return err
			}
		}
		return nil
	}
}

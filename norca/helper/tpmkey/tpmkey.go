// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package tpmkey keeps the private key of a node in its TPM 2.0. The key is
// made inside the TPM, under the storage key of the owner hierarchy, and
// never leaves it: the key file is a TSS2 PEM key file ("TSS2 PRIVATE KEY",
// the format of the tpm2 providers of OpenSSL), a blob that only the TPM that
// made it can load. A Signer signs with it (crypto.Signer), for a
// certificate request or as the key of a tls.Certificate.
//
// The key is loaded transiently, never made persistent: no NV slot of the
// TPM is used, and a rotated key is simply a replaced file. A Signer keeps
// the device open and the key loaded between signatures; with the resource
// manager of the kernel (/dev/tpmrm0) everything it loaded is flushed when
// the device is closed.
//
// The agent loads such a key as its TLS key (structs/config KeyLoader).
// norca-join, which makes the keys of the nodes it joins, has the same
// package (norca-platform, nodescaler/tpmkey): keep the two in step.
package tpmkey

import (
	"crypto"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"

	keyfile "github.com/foxboron/go-tpm-keyfiles"
	"github.com/google/go-tpm/tpm2"
	"github.com/google/go-tpm/tpm2/transport"
)

// PEMType is the type of the PEM block of a TSS2 key file.
const PEMType = "TSS2 PRIVATE KEY"

// DefaultDevice is the TPM 2.0 resource manager device of Linux.
const DefaultDevice = "/dev/tpmrm0"

// Opener opens the TPM.
type Opener func() (transport.TPMCloser, error)

// ErrUnsupported is a TPM key on a system other than Linux.
var ErrUnsupported = errors.New("TPM keys are supported on Linux only")

// IsPEM reports whether data is a TSS2 PEM key file.
func IsPEM(data []byte) bool {
	block, _ := pem.Decode(data)
	return block != nil && block.Type == PEMType
}

// Generate makes an ECDSA P-256 signing key in the TPM and returns its
// Signer and its TSS2 PEM key file.
func Generate(open Opener) (*Signer, []byte, error) {
	tpm, err := open()
	if err != nil {
		return nil, nil, fmt.Errorf("opening the TPM: %w", err)
	}
	key, err := keyfile.NewLoadableKey(tpm, tpm2.TPMAlgECC, 256, nil,
		keyfile.WithDescription("norca node key"))
	if err != nil {
		_ = tpm.Close()
		return nil, nil, fmt.Errorf("making the key in the TPM: %w", err)
	}
	s, err := newSigner(key, open)
	if err != nil {
		_ = tpm.Close()
		return nil, nil, err
	}
	// The device stays open for the first signature (the certificate
	// request).
	s.st.tpm = tpm
	return s, key.Bytes(), nil
}

// NewSigner returns the Signer of a TSS2 PEM key file. The TPM is opened at
// the first signature or by Check.
func NewSigner(keyPEM []byte, open Opener) (*Signer, error) {
	key, err := keyfile.Decode(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("the TPM key file: %w", err)
	}
	return newSigner(key, open)
}

// X509KeyPair is tls.X509KeyPair for a TSS2 PEM key file: the private key of
// the certificate is a Signer of the TPM. The TPM is opened at the first
// signature; Check the Signer to load the key at once.
func X509KeyPair(certPEM, keyPEM []byte, open Opener) (tls.Certificate, error) {
	var cert tls.Certificate
	for rest := certPEM; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			cert.Certificate = append(cert.Certificate, block.Bytes)
		}
	}
	if len(cert.Certificate) == 0 {
		return tls.Certificate{}, errors.New("the certificate file has no certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return tls.Certificate{}, err
	}
	s, err := NewSigner(keyPEM, open)
	if err != nil {
		return tls.Certificate{}, err
	}
	pub, ok := leaf.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(s.Public()) {
		_ = s.Close()
		return tls.Certificate{}, errors.New("the certificate is not for the TPM key")
	}
	cert.PrivateKey = s
	cert.Leaf = leaf
	return cert, nil
}

// Signer is a key of the TPM as a crypto.Signer. It is safe for concurrent
// use: the signatures are serialized, as the TPM serializes its commands.
type Signer struct {
	pub crypto.PublicKey
	st  *state
}

var _ crypto.Signer = (*Signer)(nil)

// state is what a Signer holds in the TPM, apart from the Signer so that
// the device is closed when the Signer is collected.
type state struct {
	mu   sync.Mutex
	open Opener
	key  *keyfile.TPMKey
	tpm  transport.TPMCloser
	obj  *tpm2.AuthHandle
}

func newSigner(key *keyfile.TPMKey, open Opener) (*Signer, error) {
	if !key.Keytype.Equal(keyfile.OIDLoadableKey) {
		return nil, errors.New("the TPM key file is not a loadable key")
	}
	if !key.HasSigner() {
		return nil, errors.New("the TPM key can not sign")
	}
	if !key.EmptyAuth {
		return nil, errors.New("the TPM key has a password: not supported")
	}
	pub, err := key.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("the public key of the TPM key: %w", err)
	}
	s := &Signer{pub: pub, st: &state{open: open, key: key}}
	// A Signer replaced by a reload is not closed by its user (a TLS
	// handshake may still use it): the device closes with it.
	runtime.AddCleanup(s, func(st *state) { _ = st.close() }, s.st)
	return s, nil
}

// Public implements crypto.Signer.
func (s *Signer) Public() crypto.PublicKey { return s.pub }

// Sign implements crypto.Signer: digest is signed by the TPM. ECDSA
// signatures are ASN.1, as crypto/ecdsa makes them.
func (s *Signer) Sign(_ io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	var hashAlg tpm2.TPMAlgID
	switch opts.HashFunc() {
	case crypto.SHA256:
		hashAlg = tpm2.TPMAlgSHA256
	case crypto.SHA384:
		hashAlg = tpm2.TPMAlgSHA384
	case crypto.SHA512:
		hashAlg = tpm2.TPMAlgSHA512
	default:
		return nil, fmt.Errorf("the TPM key does not sign with the hash %v", opts.HashFunc())
	}
	if len(digest) != opts.HashFunc().Size() {
		return nil, fmt.Errorf("a %v digest has %d bytes, not %d", opts.HashFunc(), opts.HashFunc().Size(), len(digest))
	}
	sigAlg := s.st.key.KeyAlgo()
	if _, pss := opts.(*rsa.PSSOptions); pss {
		if sigAlg != tpm2.TPMAlgRSA {
			return nil, errors.New("RSA-PSS with a key that is not RSA")
		}
		sigAlg = tpm2.TPMAlgRSAPSS
	}
	return s.st.sign(digest, hashAlg, sigAlg)
}

// Check opens the TPM and loads the key, if that is not done yet: an error
// is a key that this TPM can not use.
func (s *Signer) Check() error {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	if s.st.obj != nil {
		return nil
	}
	if err := s.st.load(); err != nil {
		_ = s.st.closeLocked()
		return err
	}
	return nil
}

// Close flushes the key from the TPM and closes the device. A later
// signature opens it again.
func (s *Signer) Close() error { return s.st.close() }

func (st *state) sign(digest []byte, hashAlg, sigAlg tpm2.TPMAlgID) ([]byte, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	var lastErr error
	// A failed signature is tried once more on a newly opened device: the
	// TPM may have been reset, or the handle lost.
	for range 2 {
		if st.obj == nil {
			if err := st.load(); err != nil {
				_ = st.closeLocked()
				return nil, err
			}
		}
		rsp, err := keyfile.TPMSign(st.tpm, *st.obj, digest, hashAlg, st.key.KeySize(), sigAlg)
		if err == nil {
			return keyfile.EncodeSignatureASN1(rsp)
		}
		lastErr = err
		_ = st.closeLocked()
	}
	return nil, fmt.Errorf("signing with the TPM key: %w", lastErr)
}

// load opens the device if needed and loads the key under the storage key,
// which is flushed at once: the key alone stays loaded.
func (st *state) load() error {
	if st.tpm == nil {
		tpm, err := st.open()
		if err != nil {
			return fmt.Errorf("opening the TPM: %w", err)
		}
		st.tpm = tpm
	}
	obj, parent, err := keyfile.LoadKey(keyfile.NewTPMSession(st.tpm), st.key, nil)
	if parent != nil && isTransient(parent.Handle) {
		keyfile.FlushHandle(st.tpm, parent)
	}
	if err != nil {
		return fmt.Errorf("loading the TPM key: %w", err)
	}
	st.obj = obj
	return nil
}

func (st *state) close() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.closeLocked()
}

func (st *state) closeLocked() error {
	if st.tpm == nil {
		st.obj = nil
		return nil
	}
	if st.obj != nil {
		keyfile.FlushHandle(st.tpm, st.obj)
		st.obj = nil
	}
	err := st.tpm.Close()
	st.tpm = nil
	return err
}

// isTransient reports whether a handle is of a transient object.
func isTransient(h tpm2.TPMHandle) bool { return uint32(h)>>24 == 0x80 }

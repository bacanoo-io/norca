// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Environment variables the Nomad client sets for tasks of task groups with
// an mtls service.
const (
	EnvCertFile = "NOMAD_MTLS_CERT_FILE"
	EnvKeyFile  = "NOMAD_MTLS_KEY_FILE"
	EnvCAFile   = "NOMAD_MTLS_CA_FILE"
	EnvSPIFFEID = "NOMAD_MTLS_SPIFFE_ID"

	// Dir is the directory, relative to the secrets directory of the
	// task, holding the files.
	Dir      = "mtls"
	CertFile = "cert.pem"
	KeyFile  = "key.pem"
	CAFile   = "bundle.pem"
)

// reloadInterval is how often the files are checked for changes.
var reloadInterval = 5 * time.Second

// FileSource loads the workload certificate, its key and the trust bundle
// from files and reloads them when the Nomad client renews them.
type FileSource struct {
	certFile, keyFile, caFile string

	mu        sync.Mutex
	checked   time.Time
	stamp     [3]fileStamp
	cert      *tls.Certificate
	leaf      *x509.Certificate
	bundle    *Bundle
	id        ID
	services  []string
	loadError error
}

type fileStamp struct {
	mod  time.Time
	size int64
}

// NewFileSource loads the given files.
func NewFileSource(certFile, keyFile, caFile string) (*FileSource, error) {
	s := &FileSource{certFile: certFile, keyFile: keyFile, caFile: caFile}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// NewFileSourceFromEnv loads the files named by the NOMAD_MTLS_* environment
// variables, or found in the mtls directory of NOMAD_SECRETS_DIR.
func NewFileSourceFromEnv() (*FileSource, error) {
	cert, key, ca := os.Getenv(EnvCertFile), os.Getenv(EnvKeyFile), os.Getenv(EnvCAFile)
	if cert == "" || key == "" || ca == "" {
		secrets := os.Getenv("NOMAD_SECRETS_DIR")
		if secrets == "" {
			return nil, errors.New("neither the NOMAD_MTLS_* nor NOMAD_SECRETS_DIR environment variables are set")
		}
		dir := filepath.Join(secrets, Dir)
		cert, key, ca = filepath.Join(dir, CertFile), filepath.Join(dir, KeyFile), filepath.Join(dir, CAFile)
	}
	return NewFileSource(cert, key, ca)
}

func (s *FileSource) stamps() ([3]fileStamp, error) {
	var out [3]fileStamp
	for i, path := range []string{s.certFile, s.keyFile, s.caFile} {
		info, err := os.Stat(path)
		if err != nil {
			return out, err
		}
		out[i] = fileStamp{mod: info.ModTime(), size: info.Size()}
	}
	return out, nil
}

// load reads the files. The caller holds the lock, or the source is not
// shared yet.
func (s *FileSource) load() error {
	stamp, err := s.stamps()
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		return fmt.Errorf("failed to load the workload certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return err
	}
	cert.Leaf = leaf
	id, err := IDFromCertificate(leaf)
	if err != nil {
		return err
	}
	caPEM, err := os.ReadFile(s.caFile)
	if err != nil {
		return err
	}
	bundle, err := ParseBundle(caPEM)
	if err != nil {
		return err
	}
	s.cert, s.leaf, s.bundle, s.id = &cert, leaf, bundle, id
	s.services = ServicesFromCertificate(leaf, id.Namespace)
	s.stamp = stamp
	s.checked = time.Now()
	return nil
}

// refresh reloads the files if they changed since the last check. A failed
// reload (for example while the client replaces the files) keeps the
// previous material.
func (s *FileSource) refresh() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.checked) < reloadInterval {
		return
	}
	s.checked = time.Now()
	stamp, err := s.stamps()
	if err != nil || stamp == s.stamp {
		return
	}
	s.loadError = s.load()
}

// Reload reloads the files now.
func (s *FileSource) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadError = s.load()
	return s.loadError
}

// Certificate returns the current workload certificate.
func (s *FileSource) Certificate() *tls.Certificate {
	s.refresh()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cert
}

// Bundle returns the current trust bundle.
func (s *FileSource) Bundle() *Bundle {
	s.refresh()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bundle
}

// ID returns the SPIFFE ID of the workload.
func (s *FileSource) ID() ID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

// Services returns the services the workload certificate asserts.
func (s *FileSource) Services() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.services...)
}

// ServerConfig returns a TLS configuration for a server of the destination
// service: it requires a client certificate, verifies it against the trust
// bundle and authorizes it with the authorizer. An empty destination means
// the only service of the workload certificate. A nil authorizer only
// verifies the identity.
func (s *FileSource) ServerConfig(authz *Authorizer, destination string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		ClientAuth: tls.RequireAnyClientCert,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return s.Certificate(), nil
		},
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			peer, err := s.Bundle().VerifyRaw(rawCerts)
			if err != nil {
				return err
			}
			if authz == nil {
				return nil
			}
			dst := destination
			if dst == "" {
				services := s.Services()
				if len(services) != 1 {
					return fmt.Errorf("%w: the destination service must be set, the workload has %d services", ErrDenied, len(services))
				}
				dst = services[0]
			}
			return authz.Authorize(peer, dst)
		},
	}
}

// ClientConfig returns a TLS configuration for connecting to a service: it
// presents the workload certificate and verifies the server against the trust
// bundle by its SPIFFE ID (instead of its host name). verify, if set, checks
// the verified server identity, for example ExpectService.
func (s *FileSource) ClientConfig(verify func(*Peer) error) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// The chain and identity are verified in VerifyPeerCertificate
		// against the trust bundle; host names are not part of the
		// workload identity.
		InsecureSkipVerify: true,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return s.Certificate(), nil
		},
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			peer, err := s.Bundle().VerifyRaw(rawCerts)
			if err != nil {
				return err
			}
			if verify != nil {
				return verify(peer)
			}
			return nil
		},
	}
}

// ExpectService returns a peer check that requires the peer to assert the
// given service in the given namespace.
func ExpectService(namespace, service string) func(*Peer) error {
	return func(peer *Peer) error {
		if peer.ID.Namespace != namespace {
			return fmt.Errorf("peer %s is not in namespace %q", peer.ID, namespace)
		}
		for _, s := range peer.Services {
			if s == service {
				return nil
			}
		}
		return fmt.Errorf("peer %s is not service %q", peer.ID, service)
	}
}

// PeerFromConnectionState returns the identity of the peer of a completed
// handshake made with a configuration of this package. The chain was
// verified during the handshake; this only reads it.
func PeerFromConnectionState(cs tls.ConnectionState) (*Peer, error) {
	if len(cs.PeerCertificates) == 0 {
		return nil, errors.New("peer presented no certificate")
	}
	leaf := cs.PeerCertificates[0]
	id, err := IDFromCertificate(leaf)
	if err != nil {
		return nil, err
	}
	return &Peer{ID: id, Services: ServicesFromCertificate(leaf, id.Namespace), Certificate: leaf}, nil
}

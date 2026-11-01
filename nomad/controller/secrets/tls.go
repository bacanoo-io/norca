// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secrets

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

// serverPath is the path of the SPIFFE ID of the leader.
const serverPath = "/norca/server"

// reloadEvery is how often the files of a rotated certificate are read again.
const reloadEvery = time.Minute

// TLS names the workload certificate of the task. The client rotates the
// files; they are read again while the server runs.
type TLS struct {
	CertFile   string
	KeyFile    string
	BundleFile string

	lock   sync.Mutex
	cert   *tls.Certificate
	pool   *x509.CertPool
	loaded time.Time
}

// TLSFromEnv returns the certificate of a task with mtls { mode = "app" },
// in secrets/mtls of its secrets directory.
func TLSFromEnv() *TLS {
	dir := filepath.Join(os.Getenv("NOMAD_SECRETS_DIR"), "mtls")
	return &TLS{
		CertFile:   filepath.Join(dir, "cert.pem"),
		KeyFile:    filepath.Join(dir, "key.pem"),
		BundleFile: filepath.Join(dir, "bundle.pem"),
	}
}

func (t *TLS) load() (*tls.Certificate, *x509.CertPool, error) {
	t.lock.Lock()
	defer t.lock.Unlock()
	if t.cert != nil && time.Since(t.loaded) < reloadEvery {
		return t.cert, t.pool, nil
	}
	cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
	if err != nil {
		if t.cert != nil {
			return t.cert, t.pool, nil // the file is being rotated
		}
		return nil, nil, fmt.Errorf("loading the workload certificate: %w", err)
	}
	bundle, err := os.ReadFile(t.BundleFile)
	if err != nil {
		return nil, nil, fmt.Errorf("loading the trust bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundle) {
		return nil, nil, errors.New("the trust bundle holds no certificates")
	}
	t.cert, t.pool, t.loaded = &cert, pool, time.Now()
	return t.cert, t.pool, nil
}

// Config returns the TLS configuration of the operator: only the servers of
// the region can call it.
func (t *TLS) Config() (*tls.Config, error) {
	if _, _, err := t.load(); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			cert, _, err := t.load()
			return cert, err
		},
		// The chain is checked in VerifyConnection, against the bundle of
		// the moment: it changes when the CA rotates.
		ClientAuth: tls.RequireAnyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			_, pool, err := t.load()
			if err != nil {
				return err
			}
			return verifyServer(cs.PeerCertificates, pool)
		},
	}, nil
}

// verifyServer accepts a certificate of the CA of the region that carries the
// SPIFFE ID of the servers.
func verifyServer(chain []*x509.Certificate, roots *x509.CertPool) error {
	if len(chain) == 0 {
		return errors.New("no client certificate")
	}
	intermediates := x509.NewCertPool()
	for _, c := range chain[1:] {
		intermediates.AddCert(c)
	}
	if _, err := chain[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return err
	}
	for _, uri := range chain[0].URIs {
		if uri.Scheme == "spiffe" && uri.Path == serverPath {
			return nil
		}
	}
	return errors.New("the client is not a server of the region")
}

// Package cluster carries HA traffic between a vault's replicas: standbys
// forward requests to the active replica over a dedicated listener secured with
// mutual TLS (ADR D-021, docs/design/ha-active-standby.md).
//
// The TLS identity comes from the vault itself, not the operator's certificates:
// the active replica creates a small CA in the barrier, and every unsealed
// replica — which can read the barrier — issues itself a short-lived leaf from
// it. Only a process that unsealed the same vault can therefore connect, which
// is also what lets the active replica trust the client address a standby
// passes along.
package cluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// caPath is where the cluster CA (certificate and private key) lives in the
// barrier — encrypted at rest like every other barrier value.
const caPath = "sys/ha/cluster-ca"

// serverName is the one DNS name every replica's leaf carries. Replicas verify
// each other by CA, not by address, so the name is a constant.
const serverName = "ubixvault-cluster"

// Leaf lifetime; a leaf is reissued once half of it has passed.
const leafTTL = 24 * time.Hour

// ErrNoCA is returned while the active replica has not created the cluster CA
// yet.
var ErrNoCA = errors.New("cluster: no cluster CA yet (the active replica creates it)")

// Storage is the barrier subset the certificates need.
type Storage interface {
	Get(ctx context.Context, key string) (*storage.Entry, error)
	Put(ctx context.Context, entry *storage.Entry) error
}

type caRecord struct {
	CertPEM []byte `json:"cert_pem"`
	KeyPEM  []byte `json:"key_pem"`
}

// Certs is one replica's cluster TLS identity.
type Certs struct {
	store Storage
	now   func() time.Time

	mu     sync.Mutex
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	pool   *x509.CertPool
	leaf   *tls.Certificate
	issued time.Time
}

// NewCerts returns a replica's certificates over the barrier.
func NewCerts(store Storage) *Certs {
	return &Certs{store: store, now: time.Now}
}

// EnsureCA creates the cluster CA if the barrier has none. Only the active
// replica may call it (it writes); standbys load the CA it created.
func (c *Certs) EnsureCA(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch err := c.loadLocked(ctx); {
	case err == nil:
		return nil
	case !errors.Is(err, ErrNoCA):
		return err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("cluster: generate CA key: %w", err)
	}
	now := c.now()
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: "uBixVault cluster CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("cluster: create CA: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("cluster: encode CA key: %w", err)
	}
	blob, err := json.Marshal(caRecord{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	})
	if err != nil {
		return fmt.Errorf("cluster: encode CA: %w", err)
	}
	if err := c.store.Put(ctx, &storage.Entry{Key: caPath, Value: blob}); err != nil {
		return fmt.Errorf("cluster: persist CA: %w", err)
	}
	return c.loadLocked(ctx)
}

// Reset forgets the CA and leaf, e.g. on seal; they are reloaded from the
// barrier when next needed.
func (c *Certs) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ca, c.caKey, c.pool, c.leaf = nil, nil, nil, nil
}

// loadLocked reads the CA from the barrier if not already loaded.
func (c *Certs) loadLocked(ctx context.Context) error {
	if c.ca != nil {
		return nil
	}
	entry, err := c.store.Get(ctx, caPath)
	if err != nil {
		return fmt.Errorf("cluster: read CA: %w", err)
	}
	if entry == nil {
		return ErrNoCA
	}
	var rec caRecord
	if err := json.Unmarshal(entry.Value, &rec); err != nil {
		return fmt.Errorf("cluster: decode CA: %w", err)
	}
	certBlock, _ := pem.Decode(rec.CertPEM)
	keyBlock, _ := pem.Decode(rec.KeyPEM)
	if certBlock == nil || keyBlock == nil {
		return fmt.Errorf("cluster: decode CA: bad PEM")
	}
	ca, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return fmt.Errorf("cluster: parse CA: %w", err)
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return fmt.Errorf("cluster: parse CA key: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	c.ca, c.caKey, c.pool, c.leaf = ca, key, pool, nil
	return nil
}

// identity returns the CA pool and a current leaf, issuing a fresh leaf when
// there is none or half its lifetime has passed.
func (c *Certs) identity(ctx context.Context) (*x509.CertPool, *tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.loadLocked(ctx); err != nil {
		return nil, nil, err
	}
	now := c.now()
	if c.leaf == nil || now.Sub(c.issued) > leafTTL/2 {
		leaf, err := c.issueLocked(now)
		if err != nil {
			return nil, nil, err
		}
		c.leaf, c.issued = leaf, now
	}
	return c.pool, c.leaf, nil
}

func (c *Certs) issueLocked(now time.Time) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("cluster: generate leaf key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: serverName},
		DNSNames:     []string{serverName},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(leafTTL),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.ca, &key.PublicKey, c.caKey)
	if err != nil {
		return nil, fmt.Errorf("cluster: issue leaf: %w", err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// ServerTLS returns the cluster listener's TLS config: TLS 1.3, and a client
// certificate from the cluster CA is required.
func (c *Certs) ServerTLS() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			ctx, cancel := context.WithTimeout(hello.Context(), 5*time.Second)
			defer cancel()
			pool, leaf, err := c.identity(ctx)
			if err != nil {
				return nil, err
			}
			return &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{*leaf},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    pool,
			}, nil
		},
	}
}

// clientTLS returns the config a standby dials the active replica with.
func (c *Certs) clientTLS(ctx context.Context) (*tls.Config, error) {
	pool, leaf, err := c.identity(ctx)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		RootCAs:      pool,
		ServerName:   serverName,
		Certificates: []tls.Certificate{*leaf},
	}, nil
}

func randomSerial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return n
}

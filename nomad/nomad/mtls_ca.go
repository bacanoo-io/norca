// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
)

// Native mTLS certificate authority.
//
// Every region has its own CA, managed by the leader of the region. The CA
// certificate is public and lives in Raft (table mtls_roots); the private key
// is stored next to it, encrypted with the active root key of the keyring,
// exactly like Variables. It is therefore replicated to every server by Raft,
// included in every snapshot and backup, and can only be decrypted by servers
// that hold the keyring, which a full restore brings back first. When the root
// key rotates, the leader re-encrypts the CA key with the new one, and the
// keyring garbage collector never deletes a root key that still encrypts a CA
// key.
//
// Trust across regions: each leader periodically fetches the public roots of
// every other region of the federation (MTLS.Roots, through region forwarding)
// and stores them in its own table, so the trust bundle of every region
// contains the CAs of all regions. No region is special and a region that is
// unreachable keeps its last known roots.

var (
	// mtlsCAInterval is the period of the leader's CA maintenance and trust
	// bundle federation. Tests lower it.
	mtlsCAInterval = time.Minute

	// mtlsCARetryInterval is the period of the CA maintenance while it
	// fails, for example until the keyring is initialized.
	mtlsCARetryInterval = 2 * time.Second

	// mtlsRetiredRootGrace is how long a retired CA stays in the trust bundle
	// after the last certificate it could have signed has expired.
	mtlsRetiredRootGrace = time.Hour
)

// mtlsCA holds the state of the certificate authority on a server.
type mtlsCA struct {
	srv    *Server
	logger hclog.Logger

	// lock serializes changes of the CA (leader loop and rotation RPC).
	lock sync.Mutex

	signersLock sync.Mutex
	signers     map[string]*mtlsSigner
}

// mtlsSigner is the decrypted key of a CA.
type mtlsSigner struct {
	cert *x509.Certificate
	key  crypto.Signer

	// keyID and ciphertext identify the encrypted form the signer was
	// decrypted from, so a re-encrypted root is decrypted again.
	keyID      string
	ciphertext []byte
}

func newMTLSCA(srv *Server, logger hclog.Logger) *mtlsCA {
	return &mtlsCA{
		srv:     srv,
		logger:  logger.Named("mtls"),
		signers: make(map[string]*mtlsSigner),
	}
}

// mtlsConfig returns the finalized mtls configuration of the server, with the
// trust domain default applied.
func (s *Server) mtlsConfig() *config.MTLSConfig {
	conf := s.config.MTLSConfig.Copy()
	if conf == nil {
		conf = &config.MTLSConfig{}
	}
	if err := conf.Finalize(); err != nil {
		// The agent validates the configuration; fall back to the defaults
		// for configurations built in code.
		s.logger.Named("mtls").Warn("invalid mtls configuration, using defaults", "error", err)
		td := conf.TrustDomain
		conf = config.DefaultMTLSConfig()
		conf.TrustDomain = td
	}
	if conf.TrustDomain == "" {
		region := s.config.AuthoritativeRegion
		if region == "" {
			region = s.config.Region
		}
		conf.TrustDomain = defaultMTLSTrustDomain(region)
	}
	return conf
}

// defaultMTLSTrustDomain derives the default trust domain from a region name.
func defaultMTLSTrustDomain(region string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(region) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		b.WriteString("global")
	}
	b.WriteString(".nomad")
	return b.String()
}

// mtlsLocalRoots splits the roots of the state into the active root of the
// region, the other roots of the region and the roots of other regions.
func mtlsSplitRoots(region string, roots []*structs.MTLSRoot) (active *structs.MTLSRoot, local, foreign []*structs.MTLSRoot) {
	for _, root := range roots {
		switch {
		case root.Region != region:
			foreign = append(foreign, root)
		case root.Active && active == nil:
			active = root
		default:
			local = append(local, root)
		}
	}
	return active, local, foreign
}

// generateMTLSRoot creates a new CA for the region: an ECDSA P-256 key and a
// self-signed CA certificate carrying the SPIFFE ID of the trust domain. The
// private key is encrypted with the active root key of the keyring.
func (s *Server) generateMTLSRoot(trustDomain string, ttl time.Duration) (*structs.MTLSRoot, *mtlsSigner, error) {
	if err := structs.ValidateSPIFFETrustDomain(trustDomain); err != nil {
		return nil, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate CA key: %w", err)
	}
	id := uuid.Generate()
	now := time.Now().UTC().Truncate(time.Second)
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	skid, err := subjectKeyID(key.Public())
	if err != nil {
		return nil, nil, err
	}
	tdURI := &url.URL{Scheme: "spiffe", Host: trustDomain}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   fmt.Sprintf("Norca mTLS CA %s %s", s.config.Region, id[:8]),
			Organization: []string{"Norca"},
		},
		URIs:                  []*url.URL{tdURI},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(ttl),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
		SubjectKeyId:          skid,
		AuthorityKeyId:        skid,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	ciphertext, keyID, err := s.encrypter.Encrypt(keyDER)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to encrypt CA key: %w", err)
	}
	root := &structs.MTLSRoot{
		ID:           id,
		Region:       s.config.Region,
		TrustDomain:  trustDomain,
		RootCert:     string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		SigningKey:   ciphertext,
		SigningKeyID: keyID,
		Active:       true,
		NotBefore:    cert.NotBefore,
		NotAfter:     cert.NotAfter,
	}
	return root, &mtlsSigner{cert: cert, key: key, keyID: keyID, ciphertext: ciphertext}, nil
}

func randomSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("failed to generate serial number: %w", err)
	}
	return serial, nil
}

// subjectKeyID computes a key identifier: the SHA-1 hash of the encoded
// public key (RFC 5280 allows any method that yields unique values).
func subjectKeyID(pub crypto.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum(der)
	return sum[:], nil
}

// parseMTLSRootCert decodes the certificate of a root.
func parseMTLSRootCert(root *structs.MTLSRoot) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(root.RootCert))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("root %s has no PEM certificate", root.ID)
	}
	return x509.ParseCertificate(block.Bytes)
}

// signer returns the decrypted key of a root of the region.
func (m *mtlsCA) signer(root *structs.MTLSRoot) (*mtlsSigner, error) {
	m.signersLock.Lock()
	defer m.signersLock.Unlock()

	if cached, ok := m.signers[root.ID]; ok && cached.keyID == root.SigningKeyID && bytes.Equal(cached.ciphertext, root.SigningKey) {
		return cached, nil
	}
	if len(root.SigningKey) == 0 {
		return nil, fmt.Errorf("CA %s has no signing key", root.ID)
	}
	keyDER, err := m.srv.encrypter.Decrypt(root.SigningKey, root.SigningKeyID)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt the key of CA %s with root key %s: %w", root.ID, root.SigningKeyID, err)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(keyDER)
	if err != nil {
		return nil, fmt.Errorf("invalid key of CA %s: %w", root.ID, err)
	}
	key, ok := parsed.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("invalid key type of CA %s", root.ID)
	}
	cert, err := parseMTLSRootCert(root)
	if err != nil {
		return nil, err
	}
	signer := &mtlsSigner{cert: cert, key: key, keyID: root.SigningKeyID, ciphertext: slices.Clone(root.SigningKey)}
	m.signers[root.ID] = signer
	return signer, nil
}

// forgetSigners drops the cached keys of roots that are no longer active.
func (m *mtlsCA) forgetSigners(keep string) {
	m.signersLock.Lock()
	defer m.signersLock.Unlock()
	for id := range m.signers {
		if id != keep {
			delete(m.signers, id)
		}
	}
}

// parseWorkloadCSR decodes and checks a PEM certificate signing request.
func parseWorkloadCSR(csrPEM string) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("CSR must be a PEM encoded CERTIFICATE REQUEST")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("invalid CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("invalid CSR signature: %w", err)
	}
	switch pub := csr.PublicKey.(type) {
	case *ecdsa.PublicKey:
		if pub.Curve != elliptic.P256() && pub.Curve != elliptic.P384() {
			return nil, errors.New("CSR key must use the P-256 or P-384 curve")
		}
	case ed25519.PublicKey:
	case *rsa.PublicKey:
		if pub.N.BitLen() < 2048 {
			return nil, errors.New("CSR RSA key must have at least 2048 bits")
		}
	default:
		return nil, errors.New("unsupported CSR key type")
	}
	return csr, nil
}

// signWorkloadCert issues a workload certificate for the public key of the
// CSR. The identity comes from the arguments, never from the CSR.
func signWorkloadCert(signer *mtlsSigner, csr *x509.CertificateRequest, spiffeID string, dnsNames []string, ttl time.Duration) (string, *x509.Certificate, error) {
	uri, err := url.Parse(spiffeID)
	if err != nil {
		return "", nil, fmt.Errorf("invalid SPIFFE ID: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return "", nil, err
	}
	skid, err := subjectKeyID(csr.PublicKey)
	if err != nil {
		return "", nil, err
	}
	now := time.Now().UTC().Truncate(time.Second)
	notAfter := now.Add(ttl)
	if notAfter.After(signer.cert.NotAfter) {
		notAfter = signer.cert.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		URIs:                  []*url.URL{uri},
		DNSNames:              dnsNames,
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageKeyAgreement,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		SubjectKeyId:          skid,
		AuthorityKeyId:        signer.cert.SubjectKeyId,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, signer.cert, csr.PublicKey, signer.key)
	if err != nil {
		return "", nil, fmt.Errorf("failed to sign certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return "", nil, err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), cert, nil
}

// runMTLSCA is the leader loop of the certificate authority: it creates the
// CA of the region, rotates it when needed, re-encrypts its key after a root
// key rotation, removes expired roots and copies the roots of the other
// regions.
func (s *Server) runMTLSCA(stopCh <-chan struct{}) {
	if !s.mtlsConfig().IsEnabled() {
		return
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-timer.C:
		}
		interval := mtlsCAInterval
		if s.IsLeader() {
			if err := s.mtls.reconcile(); err != nil {
				interval = min(interval, mtlsCARetryInterval)
				if errors.Is(err, errMTLSNotReady) {
					s.mtls.logger.Debug("certificate authority not ready", "error", err)
				} else {
					s.mtls.logger.Warn("certificate authority maintenance failed", "error", err)
				}
			}
			if err := s.mtls.federate(); err != nil {
				s.mtls.logger.Debug("trust bundle federation incomplete", "error", err)
			}
		}
		timer.Reset(interval)
	}
}

// errMTLSNotReady is returned while the CA can't be managed yet.
var errMTLSNotReady = errors.New("native mTLS is not ready")

// ready returns an error wrapping errMTLSNotReady if the CA can't be managed
// yet.
func (m *mtlsCA) ready() error {
	if !ServersMeetMinimumNorcaVersion(m.srv.serf.Members(), m.srv.Region(), false) {
		return fmt.Errorf("%w: all servers must be running Norca %v or later", errMTLSNotReady, minNorcaFeaturesVersion)
	}
	keyMeta, err := m.srv.State().GetActiveRootKeyMeta(nil)
	if err != nil {
		return err
	}
	if keyMeta == nil {
		return fmt.Errorf("%w: the keyring has not been initialized yet", errMTLSNotReady)
	}
	if _, err := m.srv.encrypter.GetKey(keyMeta.KeyID); err != nil {
		return fmt.Errorf("%w: the active root key is not available yet: %v", errMTLSNotReady, err)
	}
	return nil
}

// reconcile brings the CA of the region to the wanted state.
func (m *mtlsCA) reconcile() error {
	m.lock.Lock()
	defer m.lock.Unlock()

	if err := m.ready(); err != nil {
		return err
	}
	conf := m.srv.mtlsConfig()
	roots, err := m.srv.State().MTLSRoots(nil)
	if err != nil {
		return err
	}
	active, local, foreign := mtlsSplitRoots(m.srv.Region(), roots)
	now := time.Now().UTC()

	req := &structs.MTLSRootsUpsertRequest{}
	switch {
	case active == nil:
		root, _, err := m.srv.generateMTLSRoot(conf.TrustDomain, conf.CACertTTL)
		if err != nil {
			return err
		}
		req.Roots = append(req.Roots, root)
		m.logger.Info("created the certificate authority of the region", "root_id", root.ID, "trust_domain", root.TrustDomain)

	case active.TrustDomain != conf.TrustDomain || mtlsRootNeedsRotation(active, now):
		root, _, err := m.srv.generateMTLSRoot(conf.TrustDomain, conf.CACertTTL)
		if err != nil {
			return err
		}
		req.Roots = append(req.Roots, root, retireMTLSRoot(active, now))
		m.logger.Info("rotated the certificate authority of the region", "root_id", root.ID, "previous_root_id", active.ID, "trust_domain", root.TrustDomain)

	default:
		rewrapped, err := m.rewrap(active)
		if err != nil {
			return err
		}
		if rewrapped != nil {
			req.Roots = append(req.Roots, rewrapped)
			m.logger.Info("re-encrypted the key of the certificate authority with the active root key", "root_id", active.ID, "key_id", rewrapped.SigningKeyID)
		}
	}

	// Remove retired roots once no certificate they signed can be valid,
	// and expired roots of other regions. A retired root that still has a
	// key (an older server retired it) loses it.
	for _, root := range local {
		switch {
		case !now.Before(root.NotAfter),
			!root.RotatedOutAt.IsZero() && now.After(root.RotatedOutAt.Add(conf.LeafCertTTL+mtlsRetiredRootGrace)):
			req.DeleteIDs = append(req.DeleteIDs, root.ID)
		case len(root.SigningKey) > 0 || root.RotatedOutAt.IsZero():
			req.Roots = append(req.Roots, retireMTLSRoot(root, now))
		}
	}
	for _, root := range foreign {
		if !now.Before(root.NotAfter) {
			req.DeleteIDs = append(req.DeleteIDs, root.ID)
		}
	}

	if len(req.Roots) == 0 && len(req.DeleteIDs) == 0 {
		return nil
	}
	if _, _, err := m.srv.raftApply(structs.MTLSRootsUpsertRequestType|structs.IgnoreUnknownTypeFlag, req); err != nil {
		return err
	}
	for _, root := range req.Roots {
		if root.Active {
			m.forgetSigners(root.ID)
		}
	}
	return nil
}

// mtlsRootNeedsRotation returns whether two thirds of the lifetime of the CA
// certificate have passed.
func mtlsRootNeedsRotation(root *structs.MTLSRoot, now time.Time) bool {
	lifetime := root.NotAfter.Sub(root.NotBefore)
	return now.After(root.NotBefore.Add(lifetime * 2 / 3))
}

// retireMTLSRoot returns a copy of a root that no longer signs: it keeps its
// certificate in the trust bundle but loses its key.
func retireMTLSRoot(root *structs.MTLSRoot, now time.Time) *structs.MTLSRoot {
	retired := root.Copy()
	retired.Active = false
	retired.SigningKey = nil
	retired.SigningKeyID = ""
	if retired.RotatedOutAt.IsZero() {
		retired.RotatedOutAt = now
	}
	return retired
}

// rewrap re-encrypts the key of the active root with the active root key of
// the keyring, if it is encrypted with an older one. It returns nil if
// nothing has to change.
func (m *mtlsCA) rewrap(active *structs.MTLSRoot) (*structs.MTLSRoot, error) {
	keyMeta, err := m.srv.State().GetActiveRootKeyMeta(nil)
	if err != nil || keyMeta == nil || keyMeta.KeyID == active.SigningKeyID {
		return nil, err
	}
	signer, err := m.signer(active)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(signer.key)
	if err != nil {
		return nil, err
	}
	ciphertext, keyID, err := m.srv.encrypter.Encrypt(keyDER)
	if err != nil {
		return nil, err
	}
	if keyID == active.SigningKeyID {
		return nil, nil
	}
	rewrapped := active.Copy()
	rewrapped.SigningKey = ciphertext
	rewrapped.SigningKeyID = keyID
	return rewrapped, nil
}

// rotate replaces the active CA of the region with a new one. The retired CA
// stays in the trust bundle until the certificates it signed have expired,
// and the clients re-issue their certificates with the new CA.
func (m *mtlsCA) rotate() (*structs.MTLSRoot, uint64, error) {
	m.lock.Lock()
	defer m.lock.Unlock()

	if err := m.ready(); err != nil {
		return nil, 0, err
	}
	conf := m.srv.mtlsConfig()
	roots, err := m.srv.State().MTLSRoots(nil)
	if err != nil {
		return nil, 0, err
	}
	active, _, _ := mtlsSplitRoots(m.srv.Region(), roots)
	root, _, err := m.srv.generateMTLSRoot(conf.TrustDomain, conf.CACertTTL)
	if err != nil {
		return nil, 0, err
	}
	req := &structs.MTLSRootsUpsertRequest{Roots: []*structs.MTLSRoot{root}}
	if active != nil {
		req.Roots = append(req.Roots, retireMTLSRoot(active, time.Now().UTC()))
	}
	_, index, err := m.srv.raftApply(structs.MTLSRootsUpsertRequestType|structs.IgnoreUnknownTypeFlag, req)
	if err != nil {
		return nil, 0, err
	}
	m.forgetSigners(root.ID)
	m.logger.Info("rotated the certificate authority of the region on request", "root_id", root.ID)
	return root, index, nil
}

// federate copies the public roots of the other regions into the trust
// bundle of the region. A region that can't be reached keeps its last known
// roots; the roots of a region are removed when it reports none, or when
// federation is disabled.
func (m *mtlsCA) federate() error {
	if !ServersMeetMinimumNorcaVersion(m.srv.serf.Members(), m.srv.Region(), false) {
		return nil
	}
	region := m.srv.Region()
	enabled := m.srv.mtlsConfig().FederationEnabled()

	// Fetch outside the lock: the RPCs may take a while.
	fetched := make(map[string][]*structs.MTLSRoot)
	var errs []error
	if enabled {
		for _, other := range m.srv.Regions() {
			if other == region {
				continue
			}
			args := &structs.MTLSRootsRequest{QueryOptions: structs.QueryOptions{Region: other, AllowStale: true}}
			var reply structs.MTLSRootsResponse
			if err := m.srv.RPC("MTLS.Roots", args, &reply); err != nil {
				errs = append(errs, fmt.Errorf("region %s: %w", other, err))
				continue
			}
			var own []*structs.MTLSRoot
			for _, root := range reply.Roots {
				if root != nil && root.Region == other {
					own = append(own, root.Public())
				}
			}
			fetched[other] = own
		}
	}

	m.lock.Lock()
	defer m.lock.Unlock()

	roots, err := m.srv.State().MTLSRoots(nil)
	if err != nil {
		return err
	}
	_, _, foreign := mtlsSplitRoots(region, roots)
	req := mtlsFederationChanges(foreign, fetched, enabled)
	if len(req.Roots) > 0 || len(req.DeleteIDs) > 0 {
		if _, _, err := m.srv.raftApply(structs.MTLSRootsUpsertRequestType|structs.IgnoreUnknownTypeFlag, req); err != nil {
			return err
		}
		m.logger.Debug("updated the roots of other regions", "upserted", len(req.Roots), "deleted", len(req.DeleteIDs))
	}
	return errors.Join(errs...)
}

// mtlsFederationChanges compares the stored roots of other regions with the
// roots fetched from them.
func mtlsFederationChanges(stored []*structs.MTLSRoot, fetched map[string][]*structs.MTLSRoot, enabled bool) *structs.MTLSRootsUpsertRequest {
	req := &structs.MTLSRootsUpsertRequest{}
	byID := make(map[string]*structs.MTLSRoot, len(stored))
	for _, root := range stored {
		byID[root.ID] = root
		if !enabled {
			req.DeleteIDs = append(req.DeleteIDs, root.ID)
			continue
		}
		roots, ok := fetched[root.Region]
		if !ok {
			continue // unreachable: keep the last known roots
		}
		if !slices.ContainsFunc(roots, func(r *structs.MTLSRoot) bool { return r.ID == root.ID }) {
			req.DeleteIDs = append(req.DeleteIDs, root.ID)
		}
	}
	if !enabled {
		return req
	}
	for _, roots := range fetched {
		for _, root := range roots {
			existing, ok := byID[root.ID]
			if ok && existing.Region == root.Region && existing.RootCert == root.RootCert &&
				existing.Active == root.Active && existing.RotatedOutAt.Equal(root.RotatedOutAt) &&
				existing.TrustDomain == root.TrustDomain {
				continue
			}
			req.Roots = append(req.Roots, root)
		}
	}
	return req
}

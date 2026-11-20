// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/hashicorp/go-hclog"
)

// The issuing CA of the built-in certificate issuer.
//
// Every region has its own, managed by the leader, and only once a built-in
// CertificateIssuer exists. It is stored like the CA of the native mTLS (the
// certificate in Raft, the key next to it encrypted with the active root key
// of the keyring) but in a table of its own: its certificates are for DNS
// names, carry no SPIFFE identity and are trusted by whoever takes the bundle
// of /v1/certificates/roots, never by the mesh. An operator can put a CA of
// the organisation in its place (import); the leader does not rotate that.
//
// A retired CA keeps its key, encrypted, until the last certificate it
// signed has expired, so that its revocation list can still be signed.

var (
	// certificateCATTL is the validity of an issuing CA the leader creates.
	// Tests lower it.
	certificateCATTL = 5 * 365 * 24 * time.Hour

	// certificateCAGrace is how long a retired CA stays after the last
	// certificate it could have signed has expired.
	certificateCAGrace = time.Hour
)

// certificateCA holds the state of the issuing CA on a server.
type certificateCA struct {
	srv    *Server
	logger hclog.Logger

	// lock serializes changes of the CA (the operator loop and the RPCs).
	lock sync.Mutex

	signersLock sync.Mutex
	signers     map[string]*mtlsSigner
}

func newCertificateCA(srv *Server, logger hclog.Logger) *certificateCA {
	return &certificateCA{
		srv:     srv,
		logger:  logger.Named("certificates"),
		signers: make(map[string]*mtlsSigner),
	}
}

// errCertificateCANotReady is returned while the CA can't be managed yet.
var errCertificateCANotReady = errors.New("the issuing certificate authority is not ready")

// ready returns an error wrapping errCertificateCANotReady if the CA can't
// be managed yet.
func (c *certificateCA) ready() error {
	if !ServersMeetMinimumNorcaVersion(c.srv.serf.Members(), c.srv.Region(), false) {
		return fmt.Errorf("%w: all servers must be running Norca %v or later", errCertificateCANotReady, minNorcaFeaturesVersion)
	}
	keyMeta, err := c.srv.State().GetActiveRootKeyMeta(nil)
	if err != nil {
		return err
	}
	if keyMeta == nil {
		return fmt.Errorf("%w: the keyring has not been initialized yet", errCertificateCANotReady)
	}
	if _, err := c.srv.encrypter.GetKey(keyMeta.KeyID); err != nil {
		return fmt.Errorf("%w: the active root key is not available yet: %v", errCertificateCANotReady, err)
	}
	return nil
}

// splitCertificateAuthorities returns the active CA of the region and its
// other CAs.
func splitCertificateAuthorities(region string, all []*structs.CertificateAuthority) (active *structs.CertificateAuthority, retired []*structs.CertificateAuthority) {
	for _, ca := range all {
		switch {
		case ca.Region != region:
		case ca.Active && active == nil:
			active = ca
		default:
			retired = append(retired, ca)
		}
	}
	return active, retired
}

// parseCertificatesPEM decodes every certificate of a PEM bundle.
func parseCertificatesPEM(bundle string) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := []byte(bundle)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("unexpected PEM block %q", block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, cert)
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, errors.New("data after the last PEM block")
	}
	return out, nil
}

func encodeCertificatePEM(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// generate creates a new issuing CA of the region: an ECDSA P-256 key and a
// self-signed CA certificate that can sign leaves only. The key is encrypted
// with the active root key of the keyring.
func (c *certificateCA) generate(ttl time.Duration) (*structs.CertificateAuthority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate CA key: %w", err)
	}
	id := uuid.Generate()
	now := time.Now().UTC().Truncate(time.Second)
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	skid, err := subjectKeyID(key.Public())
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   fmt.Sprintf("Norca issuing CA %s %s", c.srv.config.Region, id[:8]),
			Organization: []string{"Norca"},
		},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(ttl),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
		SubjectKeyId:          skid,
		AuthorityKeyId:        skid,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return nil, fmt.Errorf("failed to create CA certificate: %w", err)
	}
	return c.newAuthority(id, encodeCertificatePEM(der), "", key, false)
}

// newAuthority builds the row of an active CA with its key encrypted.
func (c *certificateCA) newAuthority(id, certPEM, chainPEM string, key crypto.Signer, imported bool) (*structs.CertificateAuthority, error) {
	certs, err := parseCertificatesPEM(certPEM)
	if err != nil || len(certs) != 1 {
		return nil, fmt.Errorf("invalid CA certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	ciphertext, keyID, err := c.srv.encrypter.Encrypt(keyDER)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt CA key: %w", err)
	}
	return &structs.CertificateAuthority{
		ID:           id,
		Region:       c.srv.config.Region,
		Cert:         certPEM,
		Chain:        chainPEM,
		SigningKey:   ciphertext,
		SigningKeyID: keyID,
		Active:       true,
		Imported:     imported,
		NotBefore:    certs[0].NotBefore,
		NotAfter:     certs[0].NotAfter,
	}, nil
}

// signer returns the decrypted key of a CA of the region.
func (c *certificateCA) signer(ca *structs.CertificateAuthority) (*mtlsSigner, error) {
	c.signersLock.Lock()
	defer c.signersLock.Unlock()

	if cached, ok := c.signers[ca.ID]; ok && cached.keyID == ca.SigningKeyID && bytes.Equal(cached.ciphertext, ca.SigningKey) {
		return cached, nil
	}
	if len(ca.SigningKey) == 0 {
		return nil, fmt.Errorf("issuing CA %s has no signing key", ca.ID)
	}
	keyDER, err := c.srv.encrypter.Decrypt(ca.SigningKey, ca.SigningKeyID)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt the key of issuing CA %s with root key %s: %w", ca.ID, ca.SigningKeyID, err)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(keyDER)
	if err != nil {
		return nil, fmt.Errorf("invalid key of issuing CA %s: %w", ca.ID, err)
	}
	key, ok := parsed.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("invalid key type of issuing CA %s", ca.ID)
	}
	certs, err := parseCertificatesPEM(ca.Cert)
	if err != nil || len(certs) != 1 {
		return nil, fmt.Errorf("invalid certificate of issuing CA %s: %v", ca.ID, err)
	}
	signer := &mtlsSigner{cert: certs[0], key: key, keyID: ca.SigningKeyID, ciphertext: slices.Clone(ca.SigningKey)}
	c.signers[ca.ID] = signer
	return signer, nil
}

// forgetSigners drops the cached keys of CAs that are gone.
func (c *certificateCA) forgetSigners(keep []*structs.CertificateAuthority) {
	c.signersLock.Lock()
	defer c.signersLock.Unlock()
	for id := range c.signers {
		if !slices.ContainsFunc(keep, func(ca *structs.CertificateAuthority) bool { return ca.ID == id }) {
			delete(c.signers, id)
		}
	}
}

// certificateCANeedsRotation returns whether two thirds of the lifetime of
// the CA certificate have passed.
func certificateCANeedsRotation(ca *structs.CertificateAuthority, now time.Time) bool {
	lifetime := ca.NotAfter.Sub(ca.NotBefore)
	return now.After(ca.NotBefore.Add(lifetime * 2 / 3))
}

// retireCertificateAuthority returns a copy of a CA that no longer signs
// certificates. It keeps its key for its revocation list.
func retireCertificateAuthority(ca *structs.CertificateAuthority, now time.Time) *structs.CertificateAuthority {
	retired := ca.Copy()
	retired.Active = false
	if retired.RotatedOutAt.IsZero() {
		retired.RotatedOutAt = now
	}
	return retired
}

// rewrap re-encrypts the key of a CA with the active root key of the
// keyring, if it is encrypted with an older one. It returns nil if nothing
// has to change.
func (c *certificateCA) rewrap(ca *structs.CertificateAuthority) (*structs.CertificateAuthority, error) {
	keyMeta, err := c.srv.State().GetActiveRootKeyMeta(nil)
	if err != nil || keyMeta == nil || keyMeta.KeyID == ca.SigningKeyID || len(ca.SigningKey) == 0 {
		return nil, err
	}
	signer, err := c.signer(ca)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(signer.key)
	if err != nil {
		return nil, err
	}
	ciphertext, keyID, err := c.srv.encrypter.Encrypt(keyDER)
	if err != nil {
		return nil, err
	}
	if keyID == ca.SigningKeyID {
		return nil, nil
	}
	rewrapped := ca.Copy()
	rewrapped.SigningKey = ciphertext
	rewrapped.SigningKeyID = keyID
	return rewrapped, nil
}

// apply writes changed and deleted CAs.
func (c *certificateCA) apply(req *structs.CertificateAuthoritiesUpsertRequest) (uint64, error) {
	if len(req.Authorities) == 0 && len(req.DeleteIDs) == 0 {
		return 0, nil
	}
	req.Region = c.srv.Region()
	_, index, err := c.srv.raftApply(structs.CertificateAuthoritiesUpsertRequestType|structs.IgnoreUnknownTypeFlag, req)
	return index, err
}

// reconcile brings the issuing CA of the region to the wanted state: it
// creates one when a built-in issuer wants it, rotates it, re-encrypts the
// keys after a root key rotation, and removes retired CAs and expired
// revocations.
func (c *certificateCA) reconcile(wanted bool) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	all, err := c.srv.State().CertificateAuthorities(nil)
	if err != nil {
		return err
	}
	active, retired := splitCertificateAuthorities(c.srv.Region(), all)
	if active == nil && len(retired) == 0 && !wanted {
		return nil
	}
	if err := c.ready(); err != nil {
		return err
	}
	now := time.Now().UTC()
	req := &structs.CertificateAuthoritiesUpsertRequest{}

	// changed returns the copy of a CA the request already carries, or a new
	// one, so that several changes of one CA end in one row.
	changed := func(ca *structs.CertificateAuthority) *structs.CertificateAuthority {
		for _, c := range req.Authorities {
			if c.ID == ca.ID {
				return c
			}
		}
		cp := ca.Copy()
		req.Authorities = append(req.Authorities, cp)
		return cp
	}

	switch {
	case active == nil && wanted:
		ca, err := c.generate(certificateCATTL)
		if err != nil {
			return err
		}
		req.Authorities = append(req.Authorities, ca)
		c.logger.Info("created the issuing certificate authority of the region", "authority_id", ca.ID)

	case active != nil && !active.Imported && certificateCANeedsRotation(active, now):
		ca, err := c.generate(certificateCATTL)
		if err != nil {
			return err
		}
		req.Authorities = append(req.Authorities, ca, retireCertificateAuthority(active, now))
		c.logger.Info("rotated the issuing certificate authority of the region", "authority_id", ca.ID, "previous_authority_id", active.ID)
	}

	for _, ca := range all {
		if ca.Region != c.srv.Region() {
			continue
		}
		if !ca.Active {
			// A retired CA goes when no certificate it signed can be valid.
			if !now.Before(ca.NotAfter) ||
				(!ca.RotatedOutAt.IsZero() && now.After(ca.RotatedOutAt.Add(structs.CertificateMaxValidity+certificateCAGrace))) {
				req.DeleteIDs = append(req.DeleteIDs, ca.ID)
				continue
			}
		}
		rewrapped, err := c.rewrap(ca)
		if err != nil {
			return err
		}
		if rewrapped != nil {
			cp := changed(ca)
			cp.SigningKey, cp.SigningKeyID = rewrapped.SigningKey, rewrapped.SigningKeyID
			c.logger.Info("re-encrypted the key of the issuing certificate authority with the active root key",
				"authority_id", ca.ID, "key_id", rewrapped.SigningKeyID)
		}
		if slices.ContainsFunc(ca.Revoked, func(r *structs.CertificateRevocation) bool { return !now.Before(r.NotAfter) }) {
			cp := changed(ca)
			cp.Revoked = slices.DeleteFunc(cp.Revoked, func(r *structs.CertificateRevocation) bool { return !now.Before(r.NotAfter) })
		}
	}

	if _, err := c.apply(req); err != nil {
		return err
	}
	if len(req.DeleteIDs) > 0 {
		if left, err := c.srv.State().CertificateAuthorities(nil); err == nil {
			c.forgetSigners(left)
		}
	}
	return nil
}

// active returns the CA that signs, or nil.
func (c *certificateCA) active() (*structs.CertificateAuthority, error) {
	all, err := c.srv.State().CertificateAuthorities(nil)
	if err != nil {
		return nil, err
	}
	active, _ := splitCertificateAuthorities(c.srv.Region(), all)
	return active, nil
}

// rotate replaces the active CA of the region with a new one of the leader.
// The retired CA stays in the trust bundle until the certificates it signed
// have expired; the operator issues the certificates again with the new one.
func (c *certificateCA) rotate() (*structs.CertificateAuthority, uint64, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if err := c.ready(); err != nil {
		return nil, 0, err
	}
	active, err := c.active()
	if err != nil {
		return nil, 0, err
	}
	ca, err := c.generate(certificateCATTL)
	if err != nil {
		return nil, 0, err
	}
	req := &structs.CertificateAuthoritiesUpsertRequest{Authorities: []*structs.CertificateAuthority{ca}}
	if active != nil {
		req.Authorities = append(req.Authorities, retireCertificateAuthority(active, time.Now().UTC()))
	}
	index, err := c.apply(req)
	if err != nil {
		return nil, 0, err
	}
	c.logger.Info("rotated the issuing certificate authority of the region on request", "authority_id", ca.ID)
	return ca, index, nil
}

// parsePrivateKeyPEM decodes a PEM private key in PKCS #8, PKCS #1 or SEC 1
// form.
func parsePrivateKeyPEM(keyPEM string) (crypto.Signer, error) {
	block, rest := pem.Decode([]byte(keyPEM))
	if block == nil || len(bytes.TrimSpace(rest)) > 0 {
		return nil, errors.New("the key must be one PEM block")
	}
	var parsed any
	var err error
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		parsed, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("unsupported PEM block %q for a private key", block.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %v", err)
	}
	key, ok := parsed.(crypto.Signer)
	if !ok {
		return nil, errors.New("unsupported private key type")
	}
	return key, nil
}

// publicKeysEqual reports whether two public keys are the same key.
func publicKeysEqual(a, b crypto.PublicKey) bool {
	type equaler interface{ Equal(crypto.PublicKey) bool }
	ae, ok := a.(equaler)
	return ok && ae.Equal(b)
}

// checkImportedCA checks the certificates and the key of a CA an operator
// brings, and returns the certificate of the CA and of what is above it.
func checkImportedCA(certPEM, keyPEM string, now time.Time) (ca *x509.Certificate, above []*x509.Certificate, key crypto.Signer, err error) {
	certs, err := parseCertificatesPEM(certPEM)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid certificate: %v", err)
	}
	if len(certs) == 0 {
		return nil, nil, nil, errors.New("no certificate")
	}
	ca, above = certs[0], certs[1:]
	switch {
	case !ca.IsCA || !ca.BasicConstraintsValid:
		return nil, nil, nil, errors.New("the certificate is not a CA certificate")
	case ca.KeyUsage != 0 && ca.KeyUsage&x509.KeyUsageCertSign == 0:
		return nil, nil, nil, errors.New("the key usage of the certificate does not allow signing certificates")
	case !now.Before(ca.NotAfter):
		return nil, nil, nil, errors.New("the certificate has expired")
	case now.Before(ca.NotBefore):
		return nil, nil, nil, errors.New("the certificate is not valid yet")
	}
	if key, err = parsePrivateKeyPEM(keyPEM); err != nil {
		return nil, nil, nil, err
	}
	if !publicKeysEqual(key.Public(), ca.PublicKey) {
		return nil, nil, nil, errors.New("the key is not the key of the certificate")
	}

	// The certificates above it must lead from the CA to a self-signed
	// root, in order.
	selfSigned := bytes.Equal(ca.RawIssuer, ca.RawSubject) && ca.CheckSignatureFrom(ca) == nil
	if selfSigned {
		if len(above) > 0 {
			return nil, nil, nil, errors.New("a self-signed CA certificate must not be followed by other certificates")
		}
		return ca, nil, key, nil
	}
	if len(above) == 0 {
		return nil, nil, nil, errors.New("the CA certificate is not self-signed: the certificates above it must follow it, the root last")
	}
	child := ca
	for i, parent := range above {
		if err := child.CheckSignatureFrom(parent); err != nil {
			return nil, nil, nil, fmt.Errorf("certificate %d of the chain does not sign the one before it: %v", i+2, err)
		}
		child = parent
	}
	if child.CheckSignatureFrom(child) != nil {
		return nil, nil, nil, errors.New("the last certificate of the chain is not a self-signed root")
	}
	return ca, above, key, nil
}

// importCA puts the CA of an operator in the place of the issuing CA.
func (c *certificateCA) importCA(certPEM, keyPEM string) (*structs.CertificateAuthority, uint64, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if err := c.ready(); err != nil {
		return nil, 0, err
	}
	now := time.Now().UTC()
	caCert, above, key, err := checkImportedCA(certPEM, keyPEM, now)
	if err != nil {
		return nil, 0, structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}
	var chain strings.Builder
	for _, cert := range above {
		chain.WriteString(encodeCertificatePEM(cert.Raw))
	}
	ca, err := c.newAuthority(uuid.Generate(), encodeCertificatePEM(caCert.Raw), chain.String(), key, true)
	if err != nil {
		return nil, 0, err
	}
	active, err := c.active()
	if err != nil {
		return nil, 0, err
	}
	req := &structs.CertificateAuthoritiesUpsertRequest{Authorities: []*structs.CertificateAuthority{ca}}
	if active != nil {
		req.Authorities = append(req.Authorities, retireCertificateAuthority(active, now))
	}
	index, err := c.apply(req)
	if err != nil {
		return nil, 0, err
	}
	c.logger.Info("imported an issuing certificate authority", "authority_id", ca.ID, "subject", caCert.Subject.String())
	return ca, index, nil
}

// revoke records a revoked certificate with the CA that signed it. A serial
// that is revoked already keeps its entry, with the time of the revocation;
// only its reason may still change, and only to keyCompromise (review
// finding ATM-25b): a key found compromised after its certificate was
// revoked for something else is what a relying party has to learn (CA/Browser
// Forum Baseline Requirements, section 7.2.2), while any other change of the
// reason would only rewrite history. found reports whether the CA is still
// there, changed whether the call recorded anything.
func (c *certificateCA) revoke(authorityID string, rev *structs.CertificateRevocation) (found, changed bool, err error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	all, err := c.srv.State().CertificateAuthorities(nil)
	if err != nil {
		return false, false, err
	}
	for _, ca := range all {
		if ca.ID != authorityID || ca.Region != c.srv.Region() {
			continue
		}
		cp := ca.Copy()
		i := slices.IndexFunc(cp.Revoked, func(r *structs.CertificateRevocation) bool { return r.Serial == rev.Serial })
		switch {
		case i < 0:
			cp.Revoked = append(cp.Revoked, rev)
		case rev.Reason == structs.CertificateRevokeReasonKeyCompromise && cp.Revoked[i].Reason != rev.Reason:
			cp.Revoked[i].Reason = rev.Reason
		default:
			return true, false, nil
		}
		_, err := c.apply(&structs.CertificateAuthoritiesUpsertRequest{Authorities: []*structs.CertificateAuthority{cp}})
		return true, err == nil, err
	}
	return false, false, nil
}

// revocationList signs the revocation list of a CA (RFC 5280), valid for a
// day.
func (c *certificateCA) revocationList(ca *structs.CertificateAuthority) (string, error) {
	signer, err := c.signer(ca)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC().Truncate(time.Second)
	template := &x509.RevocationList{
		Number:     big.NewInt(now.Unix()),
		ThisUpdate: now,
		NextUpdate: now.Add(24 * time.Hour),
	}
	for _, rev := range ca.Revoked {
		serial, ok := new(big.Int).SetString(rev.Serial, 16)
		if !ok {
			continue
		}
		// The code of the reason; 0, unspecified, leaves the extension out
		// (RFC 5280, section 5.3.1).
		reason, _ := structs.CertificateRevokeReasonCode(rev.Reason)
		template.RevokedCertificateEntries = append(template.RevokedCertificateEntries, x509.RevocationListEntry{
			SerialNumber:   serial,
			RevocationTime: rev.RevokedAt,
			ReasonCode:     reason,
		})
	}
	der, err := x509.CreateRevocationList(rand.Reader, template, signer.cert, signer.key)
	if err != nil {
		return "", fmt.Errorf("failed to sign the revocation list: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: der})), nil
}

// certificateRoot returns the PEM certificate a client has to trust for the
// certificates of a CA: the CA itself, or the root above an imported one.
func certificateRoot(ca *structs.CertificateAuthority) string {
	if ca.Chain == "" {
		return ca.Cert
	}
	certs, err := parseCertificatesPEM(ca.Chain)
	if err != nil || len(certs) == 0 {
		return ca.Cert
	}
	return encodeCertificatePEM(certs[len(certs)-1].Raw)
}

// certificateIntermediates returns the PEM certificates that go with a leaf
// of the CA: nothing for a self-signed CA, otherwise the CA and what is
// above it, without the root.
func certificateIntermediates(ca *structs.CertificateAuthority) string {
	if ca.Chain == "" {
		return ""
	}
	certs, err := parseCertificatesPEM(ca.Chain)
	if err != nil || len(certs) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString(ca.Cert)
	for _, cert := range certs[:len(certs)-1] {
		out.WriteString(encodeCertificatePEM(cert.Raw))
	}
	return out.String()
}

// certificateTrustBundle returns the roots of the CAs of the region, the
// active one first, without duplicates.
func certificateTrustBundle(region string, all []*structs.CertificateAuthority) string {
	active, retired := splitCertificateAuthorities(region, all)
	slices.SortFunc(retired, func(a, b *structs.CertificateAuthority) int { return b.NotBefore.Compare(a.NotBefore) })
	var out strings.Builder
	seen := map[string]bool{}
	add := func(ca *structs.CertificateAuthority) {
		if ca == nil {
			return
		}
		root := certificateRoot(ca)
		if !seen[root] {
			seen[root] = true
			out.WriteString(root)
		}
	}
	add(active)
	for _, ca := range retired {
		add(ca)
	}
	return out.String()
}

// generateCertificateKey creates the private key of a certificate.
func generateCertificateKey(keyType string) (crypto.Signer, error) {
	switch keyType {
	case "", structs.CertificateDefaultKeyType:
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case "ecdsa-p384":
		return ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	case "rsa-2048":
		return rsa.GenerateKey(rand.Reader, 2048)
	case "rsa-4096":
		return rsa.GenerateKey(rand.Reader, 4096)
	case "ed25519":
		_, key, err := ed25519.GenerateKey(rand.Reader)
		return key, err
	}
	return nil, fmt.Errorf("unsupported key type %q", keyType)
}

// encodePrivateKeyPEM encodes a private key as a PKCS #8 PEM block.
func encodePrivateKeyPEM(key crypto.Signer) (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// certificateKeyUsage returns the key usage of a leaf for its key: an RSA
// key may also be used for key encipherment.
func certificateKeyUsage(pub crypto.PublicKey) x509.KeyUsage {
	if _, ok := pub.(*rsa.PublicKey); ok {
		return x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment
	}
	return x509.KeyUsageDigitalSignature
}

// signCertificate issues a certificate of the built-in issuer for a public
// key and the names of a spec.
func signCertificate(signer *mtlsSigner, pub crypto.PublicKey, spec *structs.CertificateSpec, validity time.Duration) (string, *x509.Certificate, error) {
	serial, err := randomSerial()
	if err != nil {
		return "", nil, err
	}
	skid, err := subjectKeyID(pub)
	if err != nil {
		return "", nil, err
	}
	now := time.Now().UTC().Truncate(time.Second)
	notAfter := now.Add(validity)
	if notAfter.After(signer.cert.NotAfter) {
		notAfter = signer.cert.NotAfter
	}
	if !notAfter.After(now) {
		// A leaf never outlives its CA: one of an expired CA would end
		// before it begins.
		return "", nil, fmt.Errorf("the issuing CA has expired (%s)", signer.cert.NotAfter.UTC().Format(time.RFC3339))
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		DNSNames:              slices.Clone(spec.DNSNames),
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              notAfter,
		KeyUsage:              certificateKeyUsage(pub),
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		SubjectKeyId:          skid,
		AuthorityKeyId:        signer.cert.SubjectKeyId,
	}
	for _, addr := range spec.IPAddresses {
		if ip := net.ParseIP(addr); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		}
	}
	// The names are in the subject alternative names; the common name is
	// only for tools that show it, and only when it fits.
	if len(spec.DNSNames) > 0 && len(spec.DNSNames[0]) <= 64 {
		template.Subject = pkix.Name{CommonName: spec.DNSNames[0]}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, signer.cert, pub, signer.key)
	if err != nil {
		return "", nil, fmt.Errorf("failed to sign certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return "", nil, err
	}
	return encodeCertificatePEM(der), cert, nil
}

// certificateSerial returns the serial number of a certificate as the status
// and the revocations write it.
func certificateSerial(cert *x509.Certificate) string {
	return hex.EncodeToString(cert.SerialNumber.Bytes())
}

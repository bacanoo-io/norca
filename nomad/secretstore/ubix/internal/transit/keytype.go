package transit

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
)

// Key types. A key is either symmetric (AES-256-GCM, used by encrypt/decrypt/
// hmac) or a signing key (used by sign/verify). An absent/empty type on a stored
// key predates typed keys and is treated as AES-256.
const (
	KeyTypeAES256    = "aes256-gcm96"
	KeyTypeECDSAP256 = "ecdsa-p256"
	KeyTypeECDSAP384 = "ecdsa-p384"
	KeyTypeECDSAP521 = "ecdsa-p521"
	KeyTypeEd25519   = "ed25519"
)

// Errors for typed keys.
var (
	ErrInvalidKeyType  = errors.New("transit: unsupported key type")
	ErrKeyTypeMismatch = errors.New("transit: operation not supported for this key type")
)

func normalizeType(t string) string {
	if t == "" {
		return KeyTypeAES256
	}
	return t
}

func isSymmetric(t string) bool { return normalizeType(t) == KeyTypeAES256 }

func isSigningType(t string) bool {
	switch t {
	case KeyTypeECDSAP256, KeyTypeECDSAP384, KeyTypeECDSAP521, KeyTypeEd25519:
		return true
	}
	return false
}

// generateMaterial produces the persisted key material for a version of a key of
// the given type: a raw AES key for symmetric keys, or a PKCS#8-encoded private
// key for signing keys.
func generateMaterial(keyType string) ([]byte, error) {
	switch normalizeType(keyType) {
	case KeyTypeAES256:
		return randomKey()
	case KeyTypeECDSAP256, KeyTypeECDSAP384, KeyTypeECDSAP521:
		priv, err := ecdsa.GenerateKey(curveForType(keyType), rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("transit: generate ecdsa key: %w", err)
		}
		return x509.MarshalPKCS8PrivateKey(priv)
	case KeyTypeEd25519:
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("transit: generate ed25519 key: %w", err)
		}
		return x509.MarshalPKCS8PrivateKey(priv)
	default:
		return nil, ErrInvalidKeyType
	}
}

func curveForType(t string) elliptic.Curve {
	switch t {
	case KeyTypeECDSAP384:
		return elliptic.P384()
	case KeyTypeECDSAP521:
		return elliptic.P521()
	default:
		return elliptic.P256()
	}
}

// parsePrivateKey decodes PKCS#8 signing-key material into a crypto.Signer.
func parsePrivateKey(der []byte) (crypto.Signer, error) {
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("transit: parse private key: %w", err)
	}
	signer, ok := k.(crypto.Signer)
	if !ok {
		return nil, ErrKeyTypeMismatch
	}
	return signer, nil
}

// publicKeyPEM returns the PEM-encoded public key for signing-key material, so a
// caller can verify signatures without the vault.
func publicKeyPEM(der []byte) (string, error) {
	signer, err := parsePrivateKey(der)
	if err != nil {
		return "", err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return "", fmt.Errorf("transit: marshal public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})), nil
}

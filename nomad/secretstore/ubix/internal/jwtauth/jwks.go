package jwtauth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"math/big"
)

// jwk is one key from a JSON Web Key Set.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	N   string `json:"n"` // RSA modulus (base64url)
	E   string `json:"e"` // RSA exponent (base64url)
	Crv string `json:"crv"`
	X   string `json:"x"` // EC x (base64url)
	Y   string `json:"y"` // EC y (base64url)
}

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

// parseJWKS parses a JWK Set into public keys.
func parseJWKS(data []byte) ([]crypto.PublicKey, error) {
	var set jwkSet
	if err := json.Unmarshal(data, &set); err != nil {
		return nil, fmt.Errorf("jwtauth: parse JWKS: %w", err)
	}
	var keys []crypto.PublicKey
	for _, k := range set.Keys {
		pub, err := k.publicKey()
		if err != nil {
			continue // skip keys we can't use
		}
		keys = append(keys, pub)
	}
	return keys, nil
}

func (k jwk) publicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		n, err := b64uBig(k.N)
		if err != nil {
			return nil, err
		}
		eb, err := b64u(k.E)
		if err != nil {
			return nil, err
		}
		e := 0
		for _, b := range eb {
			e = e<<8 | int(b)
		}
		if e == 0 {
			return nil, errors.New("jwtauth: RSA exponent is zero")
		}
		return &rsa.PublicKey{N: n, E: e}, nil
	case "EC":
		curve, err := curveFor(k.Crv)
		if err != nil {
			return nil, err
		}
		x, err := b64uBig(k.X)
		if err != nil {
			return nil, err
		}
		y, err := b64uBig(k.Y)
		if err != nil {
			return nil, err
		}
		return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
	default:
		return nil, fmt.Errorf("jwtauth: unsupported key type %q", k.Kty)
	}
}

// parsePEMPublicKey parses a PEM-encoded PKIX public key.
func parsePEMPublicKey(pemStr string) (crypto.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("jwtauth: not a PEM public key")
	}
	return x509.ParsePKIXPublicKey(block.Bytes)
}

// verify checks that sig is a valid signature over signingInput for the given
// JWT alg and public key. Only RS256/384/512 and ES256/384/512 are supported.
func verify(alg string, pub crypto.PublicKey, signingInput, sig []byte) error {
	h, err := hashFor(alg)
	if err != nil {
		return err
	}
	digest := sum(h, signingInput)

	switch alg[:2] {
	case "RS":
		rsaPub, ok := pub.(*rsa.PublicKey)
		if !ok {
			return errors.New("jwtauth: key is not RSA")
		}
		return rsa.VerifyPKCS1v15(rsaPub, hashID(alg), digest, sig)
	case "ES":
		ecPub, ok := pub.(*ecdsa.PublicKey)
		if !ok {
			return errors.New("jwtauth: key is not EC")
		}
		// JWS ECDSA signatures are the raw R||S, each half the signature.
		if len(sig)%2 != 0 {
			return errors.New("jwtauth: malformed ECDSA signature")
		}
		half := len(sig) / 2
		r := new(big.Int).SetBytes(sig[:half])
		s := new(big.Int).SetBytes(sig[half:])
		if !ecdsa.Verify(ecPub, digest, r, s) {
			return errors.New("jwtauth: ECDSA verification failed")
		}
		return nil
	default:
		return fmt.Errorf("jwtauth: unsupported alg %q", alg)
	}
}

func hashFor(alg string) (func() hash.Hash, error) {
	switch alg {
	case "RS256", "ES256":
		return sha256.New, nil
	case "RS384", "ES384":
		return sha512.New384, nil
	case "RS512", "ES512":
		return sha512.New, nil
	default:
		return nil, fmt.Errorf("jwtauth: unsupported alg %q", alg)
	}
}

func hashID(alg string) crypto.Hash {
	switch alg {
	case "RS256":
		return crypto.SHA256
	case "RS384":
		return crypto.SHA384
	default:
		return crypto.SHA512
	}
}

func curveFor(crv string) (elliptic.Curve, error) {
	switch crv {
	case "P-256":
		return elliptic.P256(), nil
	case "P-384":
		return elliptic.P384(), nil
	case "P-521":
		return elliptic.P521(), nil
	default:
		return nil, fmt.Errorf("jwtauth: unsupported curve %q", crv)
	}
}

func sum(newHash func() hash.Hash, data []byte) []byte {
	h := newHash()
	h.Write(data)
	return h.Sum(nil)
}

func b64u(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

func b64uBig(s string) (*big.Int, error) {
	b, err := b64u(s)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(b), nil
}

package transit

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// Sign signs input with the signing key's latest version and returns a
// self-describing "ubix:v<N>:<base64>" signature. For ECDSA keys the input is
// hashed with algo (sha2-256/384/512; default sha2-256) and the signature is
// ASN.1 DER; Ed25519 signs the message directly and ignores algo. It fails with
// [ErrKeyTypeMismatch] on a symmetric key.
func (e *Engine) Sign(ctx context.Context, name string, input []byte, algo string) (string, error) {
	k, err := e.load(ctx, name)
	if err != nil {
		return "", err
	}
	if !isSigningType(k.Type) {
		return "", ErrKeyTypeMismatch
	}
	signer, err := parsePrivateKey(k.Versions[k.LatestVersion])
	if err != nil {
		return "", err
	}
	sig, err := signWith(signer, k.Type, input, algo)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%sv%d:%s", cipherPrefix, k.LatestVersion, base64.StdEncoding.EncodeToString(sig)), nil
}

// Verify reports whether signature is valid for input under the key version it
// names. A well-formed signature that doesn't match returns (false, nil); a
// malformed signature or a non-signing key returns an error.
func (e *Engine) Verify(ctx context.Context, name string, input []byte, signature, algo string) (bool, error) {
	version, raw, err := parseCiphertext(signature)
	if err != nil {
		return false, err
	}
	k, err := e.load(ctx, name)
	if err != nil {
		return false, err
	}
	if !isSigningType(k.Type) {
		return false, ErrKeyTypeMismatch
	}
	material, ok := k.Versions[version]
	if !ok {
		return false, nil
	}
	signer, err := parsePrivateKey(material)
	if err != nil {
		return false, err
	}
	return verifyWith(signer.Public(), k.Type, input, raw, algo)
}

func signWith(signer crypto.Signer, keyType string, input []byte, algo string) ([]byte, error) {
	if keyType == KeyTypeEd25519 {
		// Pure Ed25519 signs the message itself, no prehash.
		return signer.Sign(rand.Reader, input, crypto.Hash(0))
	}
	priv, ok := signer.(*ecdsa.PrivateKey)
	if !ok {
		return nil, ErrKeyTypeMismatch
	}
	digest, err := signDigest(algo, input)
	if err != nil {
		return nil, err
	}
	return ecdsa.SignASN1(rand.Reader, priv, digest)
}

func verifyWith(pub crypto.PublicKey, keyType string, input, sig []byte, algo string) (bool, error) {
	if keyType == KeyTypeEd25519 {
		edPub, ok := pub.(ed25519.PublicKey)
		if !ok {
			return false, ErrKeyTypeMismatch
		}
		return ed25519.Verify(edPub, input, sig), nil
	}
	ecPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return false, ErrKeyTypeMismatch
	}
	digest, err := signDigest(algo, input)
	if err != nil {
		return false, err
	}
	return ecdsa.VerifyASN1(ecPub, digest, sig), nil
}

// signDigest hashes input for ECDSA signing using the same sha2 family as HMAC.
func signDigest(algo string, input []byte) ([]byte, error) {
	hfn, err := hmacHash(algo)
	if err != nil {
		return nil, err
	}
	h := hfn()
	h.Write(input)
	return h.Sum(nil), nil
}

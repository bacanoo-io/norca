// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package signing

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/secure-systems-lab/go-securesystemslib/encrypted"
	"github.com/shoenig/test/must"
)

func TestKeyPair_RoundTrip(t *testing.T) {
	t.Parallel()
	kp, err := GenerateKeyPair([]byte("secret"))
	must.NoError(t, err)

	block, _ := pem.Decode(kp.PrivateKeyPEM)
	must.NotNil(t, block)
	must.Eq(t, PEMTypeEncryptedPrivateKey, block.Type)

	priv, err := DecryptPrivateKey(kp.PrivateKeyPEM, []byte("secret"))
	must.NoError(t, err)
	_, ok := priv.(*ecdsa.PrivateKey)
	must.True(t, ok)

	_, err = DecryptPrivateKey(kp.PrivateKeyPEM, []byte("wrong"))
	must.ErrorContains(t, err, "wrong password")

	pub, err := ParsePublicKey(kp.PublicKeyPEM)
	must.NoError(t, err)
	must.True(t, priv.Public().(*ecdsa.PublicKey).Equal(pub))
}

// TestKeyPair_CosignEncryption checks the private key encryption against the
// go-securesystemslib implementation that cosign uses to read and write keys.
func TestKeyPair_CosignEncryption(t *testing.T) {
	t.Parallel()
	password := []byte("pw")

	// Our key, decrypted the way cosign's LoadPrivateKey does.
	kp, err := GenerateKeyPair(password)
	must.NoError(t, err)
	block, _ := pem.Decode(kp.PrivateKeyPEM)
	der, err := encrypted.Decrypt(block.Bytes, password)
	must.NoError(t, err)
	key, err := x509.ParsePKCS8PrivateKey(der)
	must.NoError(t, err)
	pub, err := ParsePublicKey(kp.PublicKeyPEM)
	must.NoError(t, err)
	must.True(t, key.(*ecdsa.PrivateKey).Public().(*ecdsa.PublicKey).Equal(pub))

	// A key encrypted the way cosign's generate-key-pair does, with both the
	// current and the older PEM type.
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must.NoError(t, err)
	der, err = x509.MarshalPKCS8PrivateKey(ecKey)
	must.NoError(t, err)
	body, err := encrypted.Encrypt(der, password)
	must.NoError(t, err)
	for _, typ := range []string{PEMTypeEncryptedPrivateKey, PEMTypeEncryptedCosignPrivateKey} {
		cosignPEM := pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: body})
		signer, err := DecryptPrivateKey(cosignPEM, password)
		must.NoError(t, err)
		must.True(t, ecKey.Equal(signer))
	}
}

// TestPayload_CosignFormat checks the payload against the exact bytes of a
// cosign simple signing payload.
func TestPayload_CosignFormat(t *testing.T) {
	t.Parallel()
	b, err := NewPayload("registry.example.com:5000/packs/redis",
		"sha256:7b3ccabffc97de872a30dfd234fd972a66d247c8cfc69b0550f276481852627d")
	must.NoError(t, err)
	must.Eq(t, `{"critical":{"identity":{"docker-reference":"registry.example.com:5000/packs/redis"},`+
		`"image":{"docker-manifest-digest":"sha256:7b3ccabffc97de872a30dfd234fd972a66d247c8cfc69b0550f276481852627d"},`+
		`"type":"cosign container image signature"},"optional":null}`, string(b))

	p, err := ParsePayload(b)
	must.NoError(t, err)
	must.Eq(t, "sha256:7b3ccabffc97de872a30dfd234fd972a66d247c8cfc69b0550f276481852627d", p.Critical.Image.DockerManifestDigest)

	// Payloads with optional annotations, as cosign writes with -a, parse.
	_, err = ParsePayload([]byte(`{"critical":{"identity":{"docker-reference":"r/p"},"image":{"docker-manifest-digest":"sha256:00"},"type":"cosign container image signature"},"optional":{"a":"b"}}`))
	must.NoError(t, err)

	_, err = ParsePayload([]byte(`{"critical":{"type":"other"}}`))
	must.Error(t, err)
}

func TestSignVerify(t *testing.T) {
	t.Parallel()
	kp, err := GenerateKeyPair(nil)
	must.NoError(t, err)
	priv, err := DecryptPrivateKey(kp.PrivateKeyPEM, nil)
	must.NoError(t, err)

	dir := t.TempDir()
	pubPath := filepath.Join(dir, "cosign.pub")
	must.NoError(t, os.WriteFile(pubPath, kp.PublicKeyPEM, 0o644))
	trusted, err := LoadPublicKey(pubPath)
	must.NoError(t, err)

	other, err := GenerateKeyPair(nil)
	must.NoError(t, err)
	otherPub, err := ParsePublicKey(other.PublicKeyPEM)
	must.NoError(t, err)
	otherID, err := KeyID(otherPub)
	must.NoError(t, err)
	untrusted := &PublicKey{Key: otherPub, ID: otherID}

	digest := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	payload, err := NewPayload("example.com/packs/redis", digest)
	must.NoError(t, err)
	sig, err := Sign(priv, payload)
	must.NoError(t, err)
	sigs := []Signature{{Payload: payload, Signature: sig}}

	res := Verify(digest, sigs, []*PublicKey{untrusted, trusted})
	must.True(t, res.Verified)
	must.Eq(t, trusted.ID, res.KeyID)
	must.Eq(t, pubPath, res.KeySource)

	must.False(t, Verify(digest, sigs, []*PublicKey{untrusted}).Verified)

	// A valid signature of another digest does not count.
	must.False(t, Verify("sha256:2222222222222222222222222222222222222222222222222222222222222222", sigs, []*PublicKey{trusted}).Verified)

	// A modified payload does not verify.
	tampered := append([]byte(nil), payload...)
	tampered[len(tampered)-2] = ' '
	must.Error(t, VerifySignature(trusted.Key, tampered, sig))
}

func TestSignVerify_Ed25519(t *testing.T) {
	t.Parallel()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	must.NoError(t, err)
	enc, err := EncryptPrivateKey(priv, []byte("x"))
	must.NoError(t, err)
	signer, err := DecryptPrivateKey(enc, []byte("x"))
	must.NoError(t, err)

	sig, err := Sign(signer, []byte("payload"))
	must.NoError(t, err)
	must.NoError(t, VerifySignature(pub, []byte("payload"), sig))
}

func TestKeyID_Stable(t *testing.T) {
	t.Parallel()
	kp, err := GenerateKeyPair(nil)
	must.NoError(t, err)
	pub, err := ParsePublicKey(kp.PublicKeyPEM)
	must.NoError(t, err)
	a, err := KeyID(pub)
	must.NoError(t, err)
	b, err := KeyID(pub)
	must.NoError(t, err)
	must.Eq(t, a, b)
	must.StrHasPrefix(t, "sha256:", a)
	must.Eq(t, 71, len(a))
}

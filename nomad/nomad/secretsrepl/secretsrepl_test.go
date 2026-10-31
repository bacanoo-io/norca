// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secretsrepl

import (
	"bytes"
	"errors"
	"testing"

	"github.com/shoenig/test/must"
)

func TestSecretsRepl_SealOpen(t *testing.T) {
	key, err := GenerateKey()
	must.NoError(t, err)
	aad := AAD("region1", "region2", key.ID(), "default", "db/password", "3", "hmac-sha256:00")
	plaintext := []byte(`{"password":"canary"}`)

	sealed, err := Seal(key.PublicKey(), plaintext, aad)
	must.NoError(t, err)
	must.False(t, bytes.Contains(sealed, []byte("canary")))

	out, err := key.Open(sealed, aad)
	must.NoError(t, err)
	must.Eq(t, plaintext, out)

	// Every value gets its own ephemeral key.
	again, err := Seal(key.PublicKey(), plaintext, aad)
	must.NoError(t, err)
	must.NotEq(t, sealed, again)

	// The parsed key opens it as well.
	parsed, err := ParsePrivateKey(key.Bytes())
	must.NoError(t, err)
	must.Eq(t, key.ID(), parsed.ID())
	out, err = parsed.Open(sealed, aad)
	must.NoError(t, err)
	must.Eq(t, plaintext, out)
}

func TestSecretsRepl_WrongKeyOrData(t *testing.T) {
	region2, err := GenerateKey()
	must.NoError(t, err)
	region3, err := GenerateKey()
	must.NoError(t, err)
	must.NotEq(t, region2.ID(), region3.ID())

	aad := AAD("region1", "region2", region2.ID(), "default", "db/password", "3")
	sealed, err := Seal(region2.PublicKey(), []byte("value"), aad)
	must.NoError(t, err)

	// Another region's key cannot decrypt it.
	_, err = region3.Open(sealed, aad)
	must.True(t, errors.Is(err, ErrDecrypt))

	// Nor can the right key with the data of another secret or version.
	_, err = region2.Open(sealed, AAD("region1", "region2", region2.ID(), "default", "db/password", "4"))
	must.True(t, errors.Is(err, ErrDecrypt))
	_, err = region2.Open(sealed, AAD("region1", "region3", region2.ID(), "default", "db/password", "3"))
	must.True(t, errors.Is(err, ErrDecrypt))

	// An altered ciphertext is refused.
	for _, i := range []int{0, 5, 40, len(sealed) - 1} {
		altered := bytes.Clone(sealed)
		altered[i] ^= 1
		_, err = region2.Open(altered, aad)
		must.True(t, errors.Is(err, ErrDecrypt), must.Sprintf("byte %d", i))
	}
	_, err = region2.Open(sealed[:20], aad)
	must.True(t, errors.Is(err, ErrDecrypt))
}

func TestSecretsRepl_AADUnambiguous(t *testing.T) {
	must.NotEq(t, AAD("ab", "c"), AAD("a", "bc"))
	must.NotEq(t, AAD("a"), AAD("a", ""))
}

func TestSecretsRepl_InvalidKeys(t *testing.T) {
	_, err := ParsePrivateKey([]byte("short"))
	must.Error(t, err)
	must.Error(t, ValidatePublicKey([]byte("short")))
	_, err = Seal([]byte("short"), []byte("x"), nil)
	must.Error(t, err)
	key, err := GenerateKey()
	must.NoError(t, err)
	must.NoError(t, ValidatePublicKey(key.PublicKey()))
}

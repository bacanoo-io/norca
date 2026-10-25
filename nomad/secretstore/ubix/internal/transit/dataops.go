package transit

import (
	"context"
	"crypto/rand"
	"fmt"
)

// ErrInvalidDataKeyBits is returned for an unsupported data-key size.
var ErrInvalidDataKeyBits = fmt.Errorf("transit: data key bits must be 128, 256, or 512")

// Rewrap decrypts ciphertext and re-encrypts it under the key's latest version,
// without exposing the plaintext to the caller. Use it to upgrade ciphertexts
// after [Engine.Rotate] so old key versions can eventually be retired.
func (e *Engine) Rewrap(ctx context.Context, name, ciphertext string) (string, error) {
	return e.RewrapWithContext(ctx, name, ciphertext, nil)
}

// GenerateDataKey returns a fresh random data key of the given bit length,
// together with that key wrapped (encrypted) under the named transit key. The
// caller uses the plaintext key locally for bulk encryption and stores only the
// wrapped form, retrieving the plaintext later via [Engine.Decrypt]. bits must be
// 128, 256, or 512.
func (e *Engine) GenerateDataKey(ctx context.Context, name string, bits int) (plaintext []byte, wrapped string, err error) {
	switch bits {
	case 128, 256, 512:
	default:
		return nil, "", ErrInvalidDataKeyBits
	}
	dk := make([]byte, bits/8)
	if _, err := rand.Read(dk); err != nil {
		return nil, "", fmt.Errorf("transit: generate data key: %w", err)
	}
	wrapped, err = e.Encrypt(ctx, name, dk)
	if err != nil {
		return nil, "", err
	}
	return dk, wrapped, nil
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package seal

import (
	"bytes"
	"context"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestAEAD_RoundTrip(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()
	key, err := RandomKey()
	must.NoError(t, err)

	w, err := AEAD{}.Wrap(ctx, key, []byte("aad"))
	must.NoError(t, err)
	must.Eq(t, structs.SealTypeAEAD, w.Seal)
	must.Len(t, KeySize, w.KEK)
	must.False(t, bytes.Contains(w.Ciphertext, key))

	got, err := AEAD{}.Unwrap(ctx, w, []byte("aad"))
	must.NoError(t, err)
	must.Eq(t, key, got)

	_, err = AEAD{}.Unwrap(ctx, w, []byte("other"))
	must.Error(t, err)
}

func TestShares_GenerateUnseal(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()

	info, key, parts, err := Generate(5, 3)
	must.NoError(t, err)
	must.Len(t, 5, parts)
	must.Eq(t, 3, info.Threshold)
	must.True(t, CheckKey(info, key))
	other, _ := RandomKey()
	must.False(t, CheckKey(info, other))

	// A key wrapped with the generated key, by a seal that knows it.
	writer := NewShares()
	must.NoError(t, writer.SetKey(info, key))
	w, err := writer.Wrap(ctx, []byte("root key"), []byte("aad"))
	must.NoError(t, err)
	must.Eq(t, info.KeyID, w.KeyID)

	s := NewShares()
	s.SetInfo(info)
	must.False(t, s.Available())
	_, err = s.Unwrap(ctx, w, []byte("aad"))
	must.True(t, structs.IsErrSealed(err))
	_, err = s.Wrap(ctx, []byte("x"), nil)
	must.True(t, structs.IsErrSealed(err))

	// Fewer shares than the threshold leave it sealed; a repeated share
	// does not count.
	progress, unsealed, err := s.Unseal(parts[0], info.Threshold, nil)
	must.NoError(t, err)
	must.False(t, unsealed)
	must.Eq(t, 1, progress)
	progress, unsealed, err = s.Unseal(parts[0], info.Threshold, nil)
	must.NoError(t, err)
	must.False(t, unsealed)
	must.Eq(t, 1, progress)
	progress, unsealed, err = s.Unseal(parts[3], info.Threshold, nil)
	must.NoError(t, err)
	must.False(t, unsealed)
	must.Eq(t, 2, progress)
	must.False(t, s.Available())

	_, unsealed, err = s.Unseal(parts[4], info.Threshold, nil)
	must.NoError(t, err)
	must.True(t, unsealed)
	must.True(t, s.Available())
	plain, err := s.Unwrap(ctx, w, []byte("aad"))
	must.NoError(t, err)
	must.Eq(t, []byte("root key"), plain)

	s.Clear()
	must.False(t, s.Available())
}

func TestShares_WrongShares(t *testing.T) {
	ci.Parallel(t)

	info, _, parts, err := Generate(3, 2)
	must.NoError(t, err)
	_, _, otherParts, err := Generate(3, 2)
	must.NoError(t, err)

	s := NewShares()
	s.SetInfo(info)
	_, _, err = s.Unseal(parts[0], info.Threshold, nil)
	must.NoError(t, err)
	_, unsealed, err := s.Unseal(otherParts[1], info.Threshold, nil)
	must.ErrorIs(t, err, ErrUnsealFailed)
	must.False(t, unsealed)
	must.Eq(t, 0, s.Progress())

	_, _, err = s.Unseal([]byte("short"), info.Threshold, nil)
	must.ErrorIs(t, err, ErrInvalidShare)

	// The right shares still unseal after a failed attempt.
	_, _, err = s.Unseal(parts[1], info.Threshold, nil)
	must.NoError(t, err)
	_, unsealed, err = s.Unseal(parts[2], info.Threshold, nil)
	must.NoError(t, err)
	must.True(t, unsealed)
}

func TestShares_KeyMismatch(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()

	info1, key1, _, err := Generate(3, 2)
	must.NoError(t, err)
	info2, key2, _, err := Generate(3, 2)
	must.NoError(t, err)

	s1 := NewShares()
	must.NoError(t, s1.SetKey(info1, key1))
	w, err := s1.Wrap(ctx, []byte("k"), nil)
	must.NoError(t, err)

	s2 := NewShares()
	must.NoError(t, s2.SetKey(info2, key2))
	_, err = s2.Unwrap(ctx, w, nil)
	must.ErrorIs(t, err, ErrKeyMismatch)

	// SetKey refuses a key that does not match the key check.
	must.ErrorIs(t, NewShares().SetKey(info1, key2), ErrUnsealFailed)
}

func TestWrapAllUnwrapAny(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()

	info, key, _, err := Generate(3, 2)
	must.NoError(t, err)
	shares := NewShares()
	must.NoError(t, shares.SetKey(info, key))

	// The optional second seal: both wraps, either unwraps.
	wraps, err := WrapAll(ctx, []Seal{AEAD{}, shares}, []byte("secret"), []byte("aad"))
	must.NoError(t, err)
	must.Len(t, 2, wraps)

	lookup := func(sealed bool) func(w *structs.SealWrappedKey) Seal {
		return func(w *structs.SealWrappedKey) Seal {
			switch w.Seal {
			case structs.SealTypeAEAD:
				return AEAD{}
			case structs.SealTypeShares:
				if sealed {
					return NewShares()
				}
				return shares
			}
			return nil
		}
	}
	got, err := UnwrapAny(ctx, lookup(false), wraps, []byte("aad"))
	must.NoError(t, err)
	must.Eq(t, []byte("secret"), got)
	got, err = UnwrapAny(ctx, lookup(true), wraps, []byte("aad"))
	must.NoError(t, err)
	must.Eq(t, []byte("secret"), got)

	// Only the shares wrap and a sealed shares seal: sealed.
	_, err = UnwrapAny(ctx, lookup(true), wraps[1:], []byte("aad"))
	must.True(t, structs.IsErrSealed(err))

	_, err = WrapAll(ctx, []Seal{NewShares()}, []byte("x"), nil)
	must.True(t, structs.IsErrSealed(err))
	_, err = WrapAll(ctx, nil, []byte("x"), nil)
	must.True(t, structs.IsErrSealed(err))
}

type testKeyring struct{ seals []Seal }

func (k *testKeyring) WrapKey(ctx context.Context, pt, aad []byte) ([]*structs.SealWrappedKey, error) {
	return WrapAll(ctx, k.seals, pt, aad)
}

func (k *testKeyring) UnwrapKey(ctx context.Context, wraps []*structs.SealWrappedKey, aad []byte) ([]byte, error) {
	return UnwrapAny(ctx, func(w *structs.SealWrappedKey) Seal {
		for _, s := range k.seals {
			if s.Type() == w.Seal {
				return s
			}
		}
		return nil
	}, wraps, aad)
}

func TestCoreSeal_Rewrap(t *testing.T) {
	ci.Parallel(t)
	ctx := context.Background()

	kr := &testKeyring{seals: []Seal{AEAD{}}}
	cs := &CoreSeal{Keyring: kr}
	must.Eq(t, "external", cs.Type())

	master, _ := RandomKey()
	blob, err := cs.Wrap(ctx, master)
	must.NoError(t, err)
	got, err := cs.Unwrap(ctx, blob)
	must.NoError(t, err)
	must.Eq(t, master, got)

	// Move to shares: the envelope holds a shares wrap only, which a
	// sealed keyring can't unwrap.
	info, key, _, err := Generate(3, 2)
	must.NoError(t, err)
	shares := NewShares()
	must.NoError(t, shares.SetKey(info, key))
	kr.seals = []Seal{AEAD{}, shares}
	both, err := cs.Rewrap(ctx, blob, nil)
	must.NoError(t, err)
	kr.seals = []Seal{shares}
	rewrapped, err := cs.Rewrap(ctx, both, nil)
	must.NoError(t, err)
	env, err := ParseCoreEnvelope(rewrapped)
	must.NoError(t, err)
	must.Eq(t, []string{"shares:" + info.KeyID}, env.Seals())
	must.False(t, bytes.Contains(rewrapped, master))

	kr.seals = []Seal{NewShares()}
	_, err = cs.Unwrap(ctx, rewrapped)
	must.True(t, structs.IsErrSealed(err))
	kr.seals = []Seal{shares}
	got, err = cs.Unwrap(ctx, rewrapped)
	must.NoError(t, err)
	must.Eq(t, master, got)

	// Nothing to do when the envelope already has the wanted seals.
	same, err := cs.Rewrap(ctx, rewrapped, func(*CoreEnvelope) bool { return true })
	must.NoError(t, err)
	must.Nil(t, same)
}

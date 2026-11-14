// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bacanoo-io/norca/norca/helper/crypto"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/nats-io/nats.go/jetstream"
)

// The values of the Consul KV bucket of the system bus are encrypted with
// the keyring, as the Variables are: JetStream keeps the bucket in files on
// the disks of the servers, and a value is often a secret.
//
// A sealed value is
//
//	consulKVSealMagic | key ID (36 bytes) | nonce | ciphertext
//
// encrypted with the root key of that ID. The additional data of the cipher
// is the key ID and the NATS key of the value, so that a sealed value put
// under another key in the files of the store does not open there.
//
// A value written before the bucket was encrypted has no such beginning and
// is read as it is; it is sealed by its next write. A value that has the
// beginning and does not open is an error, never a value.
const (
	consulKVSealMagic = "\x00norca-kv-sealed-1\x00"
	consulKVKeyIDLen  = 36
)

// consulKVKeyCheckTimeout bounds the read of the bucket before a root key
// is removed by hand.
const consulKVKeyCheckTimeout = 30 * time.Second

// errConsulKVSealed is the error of a sealed value that no key at hand
// opens.
var errConsulKVSealed = errors.New("the value of the Consul KV cannot be decrypted")

// consulKVSealKeyID returns the ID of the root key a stored value is sealed
// with; false for a value stored in the clear.
func consulKVSealKeyID(stored []byte) (string, bool) {
	if !bytes.HasPrefix(stored, []byte(consulKVSealMagic)) || len(stored) < len(consulKVSealMagic)+consulKVKeyIDLen {
		return "", false
	}
	return string(stored[len(consulKVSealMagic) : len(consulKVSealMagic)+consulKVKeyIDLen]), true
}

// consulKVSealData is the additional data of a sealed value.
func consulKVSealData(keyID, natsKey string) []byte {
	return []byte(keyID + "\x00" + natsKey)
}

// sealConsulKV encrypts the value of the NATS key natsKey of the Consul KV
// bucket with the active root key.
func (e *Encrypter) sealConsulKV(natsKey string, value []byte) ([]byte, error) {
	if e.isSealed() {
		return nil, structs.ErrSealed
	}
	keyset, err := e.activeKeySet()
	if err != nil {
		return nil, err
	}
	keyID := keyset.rootKey.Meta.KeyID
	if len(keyID) != consulKVKeyIDLen {
		return nil, fmt.Errorf("the active root key has an ID of %d bytes", len(keyID))
	}
	nonce, err := crypto.Bytes(keyset.cipher.NonceSize())
	if err != nil {
		return nil, fmt.Errorf("generating a nonce: %w", err)
	}
	out := make([]byte, 0, len(consulKVSealMagic)+len(keyID)+len(nonce)+len(value)+keyset.cipher.Overhead())
	out = append(out, consulKVSealMagic...)
	out = append(out, keyID...)
	out = append(out, nonce...)
	return keyset.cipher.Seal(out, nonce, value, consulKVSealData(keyID, natsKey)), nil
}

// openConsulKV returns the value of a stored value of the NATS key natsKey
// and the ID of the root key it was sealed with; the stored value itself
// and "" when it was stored in the clear.
func (e *Encrypter) openConsulKV(natsKey string, stored []byte) ([]byte, string, error) {
	keyID, ok := consulKVSealKeyID(stored)
	if !ok {
		return stored, "", nil
	}
	if e.isSealed() {
		return nil, keyID, structs.ErrSealed
	}
	e.lock.RLock()
	defer e.lock.RUnlock()
	keyset, err := e.keysetByIDLocked(keyID)
	if err != nil {
		return nil, keyID, fmt.Errorf("%w: %v", errConsulKVSealed, err)
	}
	box := stored[len(consulKVSealMagic)+consulKVKeyIDLen:]
	nonceSize := keyset.cipher.NonceSize()
	if len(box) < nonceSize {
		return nil, keyID, fmt.Errorf("%w: it is cut short", errConsulKVSealed)
	}
	value, err := keyset.cipher.Open(nil, box[:nonceSize], box[nonceSize:], consulKVSealData(keyID, natsKey))
	if err != nil {
		return nil, keyID, fmt.Errorf("%w: %v", errConsulKVSealed, err)
	}
	if value == nil {
		value = []byte{}
	}
	return value, keyID, nil
}

// openConsulKVWith opens a stored value with the first of the keyrings that
// can: the keyring of a backup, then the one of the server.
func openConsulKVWith(natsKey string, stored []byte, keyrings ...*Encrypter) ([]byte, error) {
	err := errConsulKVSealed
	for _, keyring := range keyrings {
		if keyring == nil {
			continue
		}
		var value []byte
		if value, _, err = keyring.openConsulKV(natsKey, stored); err == nil {
			return value, nil
		}
	}
	return nil, err
}

// consulKVKeyIDs returns the IDs of the root keys the values of the Consul
// KV bucket are sealed with, each with the number of its values. Without a
// bus or a bucket there are none.
func (s *Server) consulKVKeyIDs(ctx context.Context) (map[string]int, error) {
	kv, done, err := s.consulKVBucket(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	ids := map[string]int{}
	if kv == nil {
		return ids, nil
	}
	lister, err := kv.ListKeys(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lister.Stop() }()
	for natsKey := range lister.Keys() {
		e, err := kv.Get(ctx, natsKey)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue // deleted meanwhile
		}
		if err != nil {
			return nil, fmt.Errorf("reading %q: %w", natsKey, err)
		}
		if keyID, ok := consulKVSealKeyID(e.Value()); ok {
			ids[keyID]++
		}
	}
	return ids, ctx.Err()
}

// rekeyConsulKV seals the values of the Consul KV bucket that are sealed
// with the root key keyID again, with the active key: the bucket's part of
// a full rotation of the keyring. A value changed meanwhile was sealed with
// the active key by that write and is left alone. The revision of a
// rekeyed value changes: its ModifyIndex moves and a blocking query of it
// returns, with the same value.
func (s *Server) rekeyConsulKV(ctx context.Context, keyID string) (int, error) {
	kv, done, err := s.consulKVBucket(ctx)
	if err != nil {
		return 0, err
	}
	defer done()
	if kv == nil {
		return 0, nil
	}
	lister, err := kv.ListKeys(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = lister.Stop() }()
	rekeyed := 0
	for natsKey := range lister.Keys() {
		e, err := kv.Get(ctx, natsKey)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return rekeyed, fmt.Errorf("reading %q: %w", natsKey, err)
		}
		if id, ok := consulKVSealKeyID(e.Value()); !ok || id != keyID {
			continue
		}
		value, _, err := s.encrypter.openConsulKV(natsKey, e.Value())
		if err != nil {
			return rekeyed, fmt.Errorf("%q: %w", natsKey, err)
		}
		sealed, err := s.encrypter.sealConsulKV(natsKey, value)
		if err != nil {
			return rekeyed, err
		}
		_, err = kv.Update(ctx, natsKey, sealed, e.Revision())
		switch {
		case err == nil:
			rekeyed++
		case errors.Is(err, jetstream.ErrKeyExists), errors.Is(err, jetstream.ErrKeyRevisionMismatch),
			errors.Is(err, jetstream.ErrKeyNotFound), isConsulKVWrongLastSequence(err):
			// written or deleted meanwhile
		default:
			return rekeyed, fmt.Errorf("writing %q: %w", natsKey, err)
		}
	}
	return rekeyed, ctx.Err()
}

// isConsulKVWrongLastSequence reports a failed check-and-set of the bucket.
func isConsulKVWrongLastSequence(err error) bool {
	var apiErr *jetstream.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence
}

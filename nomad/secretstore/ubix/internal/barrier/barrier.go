// Package barrier implements uBixVault's cryptographic barrier: the layer that
// encrypts all data at rest (docs/DESIGN.md §3.1).
//
// A Barrier wraps a [storage.Backend] and encrypts every value with AES-256-GCM
// before it is persisted, so the underlying store never sees plaintext. Keys
// (paths) are not encrypted — only values — matching HashiCorp Vault.
//
// The Barrier is stateful. It is *sealed* until [Barrier.Unseal] is given the
// master key; while sealed it holds ciphertext but cannot read or write data.
// This is why a Barrier is deliberately NOT a [storage.Backend]: a sealed
// barrier must fail Get/Put, which would violate the substitutability a plain
// backend promises (docs/DESIGN.md §7.1).
//
// Key hierarchy (docs/DESIGN.md §3.1): master key -> barrier key -> data. The
// master key is supplied by the caller; deriving it from Shamir unseal shares is
// a separate layer. At Initialize a random barrier key is generated and stored
// encrypted under the master key; Unseal decrypts it back into memory.
package barrier

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// KeySize is the required length in bytes of both the master key and the barrier
// key. 32 bytes selects AES-256.
const KeySize = 32

// formatVersion is the first byte of every encrypted blob. It lets the on-disk
// format evolve (docs/DESIGN.md §8.9) and is authenticated as additional data so
// it cannot be altered without failing decryption.
const formatVersion byte = 1

// Reserved storage locations used by the barrier itself. Data operations refuse
// to touch anything under reservedPrefix so callers cannot clobber the keyring.
const (
	reservedPrefix = "core/"
	keyringPath    = "core/keyring"
)

// Barrier-related errors.
var (
	ErrSealed             = errors.New("barrier: sealed")
	ErrNotInitialized     = errors.New("barrier: not initialized")
	ErrAlreadyInitialized = errors.New("barrier: already initialized")
	ErrInvalidKey         = errors.New("barrier: invalid or incorrect key")
	ErrReservedPath       = errors.New("barrier: reserved path")
	errMalformed          = errors.New("barrier: malformed encrypted entry")
)

// Barrier encrypts data at rest on top of an inner [storage.Backend].
type Barrier struct {
	phys storage.Backend

	mu   sync.RWMutex
	key  []byte      // barrier key; nil while sealed
	aead cipher.AEAD // derived from key; nil while sealed
}

// New returns a sealed Barrier over phys. Call [Barrier.Initialize] once to set
// it up, then [Barrier.Unseal] to make it usable.
func New(phys storage.Backend) *Barrier {
	return &Barrier{phys: phys}
}

// Initialize generates a fresh barrier key, encrypts it under masterKey, and
// persists it. It fails with [ErrAlreadyInitialized] if a keyring already
// exists. The Barrier remains sealed; call [Barrier.Unseal] to use it.
func (b *Barrier) Initialize(ctx context.Context, masterKey []byte) error {
	master, err := newAEAD(masterKey)
	if err != nil {
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	existing, err := b.phys.Get(ctx, keyringPath)
	if err != nil {
		return fmt.Errorf("barrier: check init: %w", err)
	}
	if existing != nil {
		return ErrAlreadyInitialized
	}

	barrierKey := make([]byte, KeySize)
	if _, err := rand.Read(barrierKey); err != nil {
		return fmt.Errorf("barrier: generate key: %w", err)
	}
	blob, err := encrypt(master, barrierKey, keyringPath)
	if err != nil {
		return fmt.Errorf("barrier: seal keyring: %w", err)
	}
	if err := b.phys.Put(ctx, &storage.Entry{Key: keyringPath, Value: blob}); err != nil {
		return fmt.Errorf("barrier: persist keyring: %w", err)
	}
	zero(barrierKey)
	return nil
}

// Initialized reports whether a keyring has been written to the backend.
func (b *Barrier) Initialized(ctx context.Context) (bool, error) {
	entry, err := b.phys.Get(ctx, keyringPath)
	if err != nil {
		return false, fmt.Errorf("barrier: check init: %w", err)
	}
	return entry != nil, nil
}

// VerifyMasterKey reports whether masterKey is the correct master key, by
// attempting to authenticate the stored keyring with it. It does not change the
// seal state, so it can validate a reconstructed key (e.g. during root
// regeneration) whether the barrier is sealed or unsealed. It returns
// [ErrNotInitialized] if no keyring exists.
func (b *Barrier) VerifyMasterKey(ctx context.Context, masterKey []byte) (bool, error) {
	master, err := newAEAD(masterKey)
	if err != nil {
		return false, err
	}
	entry, err := b.phys.Get(ctx, keyringPath)
	if err != nil {
		return false, fmt.Errorf("barrier: read keyring: %w", err)
	}
	if entry == nil {
		return false, ErrNotInitialized
	}
	if _, err := decrypt(master, entry.Value, keyringPath); err != nil {
		return false, nil // wrong key: authentication failed
	}
	return true, nil
}

// Rekey re-encrypts the stored barrier key from oldMasterKey to newMasterKey.
// This changes which master key unseals the vault without altering the barrier
// key or re-encrypting any data — the master key only wraps the keyring. It
// fails with [ErrInvalidKey] if oldMasterKey does not decrypt the keyring, and
// [ErrNotInitialized] if no keyring exists. The seal state is unchanged: an
// unsealed barrier keeps serving, since its in-memory barrier key is untouched.
func (b *Barrier) Rekey(ctx context.Context, oldMasterKey, newMasterKey []byte) error {
	oldMaster, err := newAEAD(oldMasterKey)
	if err != nil {
		return err
	}
	newMaster, err := newAEAD(newMasterKey)
	if err != nil {
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	entry, err := b.phys.Get(ctx, keyringPath)
	if err != nil {
		return fmt.Errorf("barrier: read keyring: %w", err)
	}
	if entry == nil {
		return ErrNotInitialized
	}
	barrierKey, err := decrypt(oldMaster, entry.Value, keyringPath)
	if err != nil {
		return ErrInvalidKey // wrong old master key: authentication failed
	}
	defer zero(barrierKey)

	blob, err := encrypt(newMaster, barrierKey, keyringPath)
	if err != nil {
		return fmt.Errorf("barrier: reseal keyring: %w", err)
	}
	if err := b.phys.Put(ctx, &storage.Entry{Key: keyringPath, Value: blob}); err != nil {
		return fmt.Errorf("barrier: persist keyring: %w", err)
	}
	return nil
}

// Unseal decrypts the stored barrier key with masterKey and makes the Barrier
// usable. It returns [ErrNotInitialized] if no keyring exists and [ErrInvalidKey]
// if masterKey is the wrong length or does not decrypt the keyring. Unsealing an
// already-unsealed Barrier is a no-op.
func (b *Barrier) Unseal(ctx context.Context, masterKey []byte) error {
	master, err := newAEAD(masterKey)
	if err != nil {
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.aead != nil {
		return nil // already unsealed
	}

	entry, err := b.phys.Get(ctx, keyringPath)
	if err != nil {
		return fmt.Errorf("barrier: read keyring: %w", err)
	}
	if entry == nil {
		return ErrNotInitialized
	}

	barrierKey, err := decrypt(master, entry.Value, keyringPath)
	if err != nil {
		// A wrong master key fails GCM authentication; report it as an invalid key
		// rather than leaking the cryptographic detail.
		return ErrInvalidKey
	}
	aead, err := newAEAD(barrierKey)
	if err != nil {
		zero(barrierKey)
		return fmt.Errorf("barrier: load barrier key: %w", err)
	}

	b.key = barrierKey
	b.aead = aead
	return nil
}

// Seal drops the in-memory barrier key, returning the Barrier to the sealed
// state. The key bytes are zeroed on a best-effort basis (the Go runtime may
// retain copies; see docs/DESIGN.md §2).
func (b *Barrier) Seal() {
	b.mu.Lock()
	defer b.mu.Unlock()
	zero(b.key)
	b.key = nil
	b.aead = nil
}

// Sealed reports whether the Barrier is currently sealed.
func (b *Barrier) Sealed() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.aead == nil
}

// Get returns the decrypted entry at key, or (nil, nil) if it does not exist.
func (b *Barrier) Get(ctx context.Context, key string) (*storage.Entry, error) {
	if err := checkDataKey(key); err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.aead == nil {
		return nil, ErrSealed
	}

	entry, err := b.phys.Get(ctx, key)
	if err != nil || entry == nil {
		return nil, err
	}
	plaintext, err := decrypt(b.aead, entry.Value, key)
	if err != nil {
		return nil, fmt.Errorf("barrier: decrypt %q: %w", key, err)
	}
	return &storage.Entry{Key: key, Value: plaintext}, nil
}

// Put encrypts entry.Value and stores it at entry.Key.
func (b *Barrier) Put(ctx context.Context, entry *storage.Entry) error {
	if err := checkDataKey(entry.Key); err != nil {
		return err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.aead == nil {
		return ErrSealed
	}

	blob, err := encrypt(b.aead, entry.Value, entry.Key)
	if err != nil {
		return fmt.Errorf("barrier: encrypt %q: %w", entry.Key, err)
	}
	return b.phys.Put(ctx, &storage.Entry{Key: entry.Key, Value: blob})
}

// Delete removes the value at key. It is a no-op if key does not exist.
func (b *Barrier) Delete(ctx context.Context, key string) error {
	if err := checkDataKey(key); err != nil {
		return err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.aead == nil {
		return ErrSealed
	}
	return b.phys.Delete(ctx, key)
}

// List returns the immediate children under prefix (see [storage.Backend.List]),
// omitting the barrier's own reserved entries. Key names are not encrypted.
func (b *Barrier) List(ctx context.Context, prefix string) ([]string, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.aead == nil {
		return nil, ErrSealed
	}

	children, err := b.phys.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	out := children[:0:0]
	for _, c := range children {
		if prefix == "" && c == reservedPrefix {
			continue // hide the barrier's internal namespace at the root
		}
		out = append(out, c)
	}
	return out, nil
}

// checkDataKey validates a data key and rejects the barrier's reserved namespace.
func checkDataKey(key string) error {
	if err := storage.ValidateKey(key); err != nil {
		return err
	}
	if strings.HasPrefix(key, reservedPrefix) {
		return ErrReservedPath
	}
	return nil
}

// newAEAD builds an AES-256-GCM AEAD from a 32-byte key.
func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrInvalidKey
	}
	return cipher.NewGCM(block)
}

// aad builds the GCM additional authenticated data for an entry: the format
// version byte followed by the storage path. Binding the path means a ciphertext
// only authenticates at the exact location it was written to, so an attacker with
// storage access cannot relocate a valid blob from one path to another (as
// HashiCorp Vault's barrier also does). The version byte is likewise
// authenticated, so the format cannot be silently downgraded.
func aad(path string) []byte {
	ad := make([]byte, 0, 1+len(path))
	ad = append(ad, formatVersion)
	return append(ad, path...)
}

// encrypt produces version || nonce || ciphertext+tag, binding path via the AAD.
func encrypt(aead cipher.AEAD, plaintext []byte, path string) ([]byte, error) {
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("barrier: nonce: %w", err)
	}
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+aead.Overhead())
	out = append(out, formatVersion)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plaintext, aad(path)), nil
}

// decrypt reverses encrypt, verifying the format version, path binding, and
// authentication tag. path must be the same location the blob was written to.
func decrypt(aead cipher.AEAD, blob []byte, path string) ([]byte, error) {
	ns := aead.NonceSize()
	if len(blob) < 1+ns {
		return nil, errMalformed
	}
	if blob[0] != formatVersion {
		return nil, fmt.Errorf("%w: unknown version %d", errMalformed, blob[0])
	}
	nonce := blob[1 : 1+ns]
	ciphertext := blob[1+ns:]
	return aead.Open(nil, nonce, ciphertext, aad(path))
}

// zero overwrites b with zeros (best-effort key hygiene).
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

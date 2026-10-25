// Package transit implements encryption-as-a-service (docs/DESIGN.md §3.6): named
// keys that never leave the vault, used to encrypt/decrypt data supplied by
// callers. Applications send plaintext and receive ciphertext (and vice versa)
// without ever handling key material.
//
// Keys are versioned: rotating a key adds a new version used for new encryptions
// while older versions remain able to decrypt existing ciphertext. Ciphertext is
// self-describing — "ubix:v<N>:<base64>" — so decryption selects the right key
// version. The key name is bound as additional authenticated data, so ciphertext
// produced under one key cannot be decrypted under another.
//
// Key material is stored through the barrier, so it is encrypted at rest.
package transit

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// keySize selects AES-256.
const keySize = 32

// cipherPrefix marks a transit ciphertext.
const cipherPrefix = "ubix:"

// Errors.
var (
	ErrKeyNotFound       = errors.New("transit: key not found")
	ErrKeyExists         = errors.New("transit: key already exists")
	ErrInvalidName       = errors.New("transit: invalid key name")
	ErrInvalidCiphertext = errors.New("transit: invalid ciphertext")
)

// Storage is the subset of a backend the engine needs.
type Storage interface {
	Get(ctx context.Context, key string) (*storage.Entry, error)
	Put(ctx context.Context, entry *storage.Entry) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
}

// keyData is the persisted form of a transit key, including its version material.
type keyData struct {
	Name          string         `json:"name"`
	Type          string         `json:"type,omitempty"` // "" == aes256-gcm96 (pre-typed keys)
	Versions      map[int][]byte `json:"versions"`       // version -> AES key or PKCS#8 signing key
	LatestVersion int            `json:"latest_version"`
	Derived       bool           `json:"derived,omitempty"`    // encryption key is derived per-context
	Convergent    bool           `json:"convergent,omitempty"` // deterministic ciphertext (implies Derived)
	CreatedTime   time.Time      `json:"created_time"`
}

// KeyInfo is the non-secret metadata for a key.
type KeyInfo struct {
	Name          string    `json:"name"`
	Type          string    `json:"type"`
	LatestVersion int       `json:"latest_version"`
	Versions      []int     `json:"versions"`
	Derived       bool      `json:"derived"`
	Convergent    bool      `json:"convergent"`
	CreatedTime   time.Time `json:"created_time"`
	// PublicKeys holds the PEM public key per version for signing keys; nil for
	// symmetric keys.
	PublicKeys map[int]string `json:"public_keys,omitempty"`
}

// Engine is a transit secrets engine mounted at a storage prefix.
type Engine struct {
	store  Storage
	prefix string
	now    func() time.Time
}

// New returns a transit engine storing under prefix (e.g. "transit").
func New(store Storage, prefix string) *Engine {
	return &Engine{
		store:  store,
		prefix: strings.Trim(prefix, "/"),
		now:    func() time.Time { return time.Now().UTC() },
	}
}

func (e *Engine) keyPath(name string) string { return e.prefix + "/key/" + name }

func (e *Engine) validateName(name string) error {
	if name == "" || strings.Contains(name, "/") {
		return ErrInvalidName
	}
	if storage.ValidateKey(e.keyPath(name)) != nil {
		return ErrInvalidName
	}
	return nil
}

// CreateKey creates a new symmetric (AES-256-GCM) key with a single version.
func (e *Engine) CreateKey(ctx context.Context, name string) (*KeyInfo, error) {
	return e.CreateTypedKey(ctx, name, KeyTypeAES256)
}

// CreateTypedKey creates a new key of the given type with a single version. It
// fails with [ErrKeyExists] if the name is in use, or [ErrInvalidKeyType] for an
// unknown type. keyType is one of the KeyType* constants; an empty string means
// AES-256.
func (e *Engine) CreateTypedKey(ctx context.Context, name, keyType string) (*KeyInfo, error) {
	return e.CreateKeyWithOptions(ctx, name, keyType, KeyOptions{})
}

// KeyOptions holds the optional behaviors of a new key.
type KeyOptions struct {
	// Derived makes every encrypt/decrypt derive a per-context subkey from a
	// caller-supplied context (via HKDF), so one key yields independent keys per
	// context. Symmetric keys only.
	Derived bool
	// Convergent makes encryption deterministic — the same plaintext and context
	// produce the same ciphertext, enabling equality checks without decrypting.
	// It implies Derived.
	Convergent bool
}

// CreateKeyWithOptions creates a new key of the given type with a single
// version and the given options. Convergent implies Derived, and both require a
// symmetric key.
func (e *Engine) CreateKeyWithOptions(ctx context.Context, name, keyType string, opts KeyOptions) (*KeyInfo, error) {
	if err := e.validateName(name); err != nil {
		return nil, err
	}
	if opts.Convergent {
		opts.Derived = true
	}
	if opts.Derived && !isSymmetric(keyType) {
		return nil, ErrKeyTypeMismatch
	}
	if _, err := e.load(ctx, name); err == nil {
		return nil, ErrKeyExists
	} else if !errors.Is(err, ErrKeyNotFound) {
		return nil, err
	}

	material, err := generateMaterial(keyType)
	if err != nil {
		return nil, err
	}
	k := &keyData{
		Name:          name,
		Type:          normalizeType(keyType),
		Versions:      map[int][]byte{1: material},
		LatestVersion: 1,
		Derived:       opts.Derived,
		Convergent:    opts.Convergent,
		CreatedTime:   e.now(),
	}
	if err := e.save(ctx, k); err != nil {
		return nil, err
	}
	return info(k), nil
}

// Rotate adds a new key version, which becomes the version used for encryption.
func (e *Engine) Rotate(ctx context.Context, name string) (*KeyInfo, error) {
	k, err := e.load(ctx, name)
	if err != nil {
		return nil, err
	}
	material, err := generateMaterial(k.Type)
	if err != nil {
		return nil, err
	}
	k.LatestVersion++
	k.Versions[k.LatestVersion] = material
	if err := e.save(ctx, k); err != nil {
		return nil, err
	}
	return info(k), nil
}

// ReadKey returns a key's metadata (never its material).
func (e *Engine) ReadKey(ctx context.Context, name string) (*KeyInfo, error) {
	k, err := e.load(ctx, name)
	if err != nil {
		return nil, err
	}
	return info(k), nil
}

// ListKeys returns the names of all keys.
func (e *Engine) ListKeys(ctx context.Context) ([]string, error) {
	names, err := e.store.List(ctx, e.prefix+"/key/")
	if err != nil {
		return nil, fmt.Errorf("transit: list keys: %w", err)
	}
	return names, nil
}

// DeleteKey removes a key and all its versions.
func (e *Engine) DeleteKey(ctx context.Context, name string) error {
	if err := e.validateName(name); err != nil {
		return err
	}
	if err := e.store.Delete(ctx, e.keyPath(name)); err != nil {
		return fmt.Errorf("transit: delete key: %w", err)
	}
	return nil
}

// Encrypt encrypts plaintext with the key's latest version and returns a
// self-describing ciphertext string. For a derived key, use
// [Engine.EncryptWithContext].
func (e *Engine) Encrypt(ctx context.Context, name string, plaintext []byte) (string, error) {
	return e.EncryptWithContext(ctx, name, plaintext, nil)
}

// Decrypt reverses Encrypt, selecting the key version named in the ciphertext.
func (e *Engine) Decrypt(ctx context.Context, name, ciphertext string) ([]byte, error) {
	return e.DecryptWithContext(ctx, name, ciphertext, nil)
}

func (e *Engine) load(ctx context.Context, name string) (*keyData, error) {
	if err := e.validateName(name); err != nil {
		return nil, err
	}
	entry, err := e.store.Get(ctx, e.keyPath(name))
	if err != nil {
		return nil, fmt.Errorf("transit: read key: %w", err)
	}
	if entry == nil {
		return nil, ErrKeyNotFound
	}
	var k keyData
	if err := json.Unmarshal(entry.Value, &k); err != nil {
		return nil, fmt.Errorf("transit: unmarshal key: %w", err)
	}
	return &k, nil
}

func (e *Engine) save(ctx context.Context, k *keyData) error {
	blob, err := json.Marshal(k)
	if err != nil {
		return fmt.Errorf("transit: marshal key: %w", err)
	}
	if err := e.store.Put(ctx, &storage.Entry{Key: e.keyPath(k.Name), Value: blob}); err != nil {
		return fmt.Errorf("transit: persist key: %w", err)
	}
	return nil
}

func info(k *keyData) *KeyInfo {
	versions := make([]int, 0, len(k.Versions))
	for v := range k.Versions {
		versions = append(versions, v)
	}
	sort.Ints(versions)
	ki := &KeyInfo{
		Name:          k.Name,
		Type:          normalizeType(k.Type),
		LatestVersion: k.LatestVersion,
		Versions:      versions,
		Derived:       k.Derived,
		Convergent:    k.Convergent,
		CreatedTime:   k.CreatedTime,
	}
	if isSigningType(k.Type) {
		pks := make(map[int]string, len(k.Versions))
		for v, material := range k.Versions {
			if pemStr, err := publicKeyPEM(material); err == nil {
				pks[v] = pemStr
			}
		}
		ki.PublicKeys = pks
	}
	return ki
}

func randomKey() ([]byte, error) {
	b := make([]byte, keySize)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("transit: generate key: %w", err)
	}
	return b, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("transit: cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// parseCiphertext splits "ubix:v<N>:<base64>" into its version and raw bytes.
func parseCiphertext(s string) (int, []byte, error) {
	rest, ok := strings.CutPrefix(s, cipherPrefix)
	if !ok {
		return 0, nil, ErrInvalidCiphertext
	}
	verStr, b64, ok := strings.Cut(rest, ":")
	if !ok || !strings.HasPrefix(verStr, "v") {
		return 0, nil, ErrInvalidCiphertext
	}
	version, err := strconv.Atoi(strings.TrimPrefix(verStr, "v"))
	if err != nil || version < 1 {
		return 0, nil, ErrInvalidCiphertext
	}
	blob, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return 0, nil, ErrInvalidCiphertext
	}
	return version, blob, nil
}

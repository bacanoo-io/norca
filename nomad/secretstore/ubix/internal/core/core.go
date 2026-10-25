// Package core orchestrates initialization and the seal/unseal lifecycle: it
// ties the Shamir secret-sharing scheme to the encryption barrier (docs/DESIGN.md
// §3.1). It is the logic behind the operator init / unseal / seal / status
// commands.
//
// Chain of protection: unseal shares -> master key -> barrier key -> data.
//   - Initialize generates a random master key, hands it to the barrier, splits
//     it into k-of-n Shamir shares, returns the shares, and discards the key.
//   - Unseal collects shares one at a time; once the threshold is reached it
//     reconstructs the master key and unseals the barrier.
//
// The number of shares and the threshold are persisted (unencrypted, since they
// are needed before unseal and are not secret) so Unseal knows how many shares
// to expect.
package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"io"

	"github.com/cwolsen7905/ubixvault/internal/barrier"
	"github.com/cwolsen7905/ubixvault/internal/seal"
	"github.com/cwolsen7905/ubixvault/internal/shamir"
	"github.com/cwolsen7905/ubixvault/internal/snapshot"
	"github.com/cwolsen7905/ubixvault/internal/storage"
	"github.com/cwolsen7905/ubixvault/internal/token"
)

// masterKeySize is the length of the generated master key (AES-256).
const masterKeySize = 32

// Storage locations in the barrier's reserved "core/" namespace, written through
// the physical backend directly so they remain readable while sealed.
const (
	sealConfigPath       = "core/seal-config"
	wrappedMasterKeyPath = "core/auto-master" // auto-unseal: master key wrapped by the KEK
)

// Seal types recorded in the seal config.
const (
	SealTypeShamir   = "shamir"
	SealTypeAuto     = "auto"     // auto-unseal with a locally-held KEK
	SealTypeTransit  = "transit"  // auto-unseal via a remote Transit engine
	SealTypeExternal = "external" // auto-unseal via an operator-supplied KMS/HSM command
)

// isAutoSeal reports whether a seal type is one of the auto-unseal modes (which
// share the recovery-key and root-regeneration behaviour), as opposed to Shamir.
func isAutoSeal(t string) bool {
	return t == SealTypeAuto || t == SealTypeTransit || t == SealTypeExternal
}

// Errors returned by Core.
var (
	ErrAlreadyInitialized      = errors.New("core: already initialized")
	ErrNotInitialized          = errors.New("core: not initialized")
	ErrInvalidConfig           = errors.New("core: invalid share configuration")
	ErrInvalidShare            = errors.New("core: invalid unseal share")
	ErrUnsealFailed            = errors.New("core: unseal failed (shares did not reconstruct the master key)")
	ErrAutoUnsealNotConfigured = errors.New("core: auto-unseal is not configured")
	ErrNotAutoUnseal           = errors.New("core: vault uses Shamir unseal, not auto-unseal")
	ErrAutoUnsealShamir        = errors.New("core: vault uses auto-unseal; manual unseal not applicable")
	ErrRootGenNotStarted       = errors.New("core: no root-generation attempt in progress")
	ErrRootGenNonce            = errors.New("core: nonce does not match the current attempt")
	ErrRootGenSealed           = errors.New("core: unseal the vault before regenerating root")
	ErrRootGenNotShamir        = errors.New("core: root regeneration requires Shamir unseal")
	ErrRootGenNoRecovery       = errors.New("core: this auto-unseal vault has no recovery keys (initialized before recovery-key support)")
	ErrRekeyNotStarted         = errors.New("core: no rekey attempt in progress")
	ErrRekeyNonce              = errors.New("core: nonce does not match the current rekey attempt")
	ErrRekeySealed             = errors.New("core: unseal the vault before rekeying")
	ErrRekeyNotShamir          = errors.New("core: rekey applies only to Shamir-unseal vaults")
)

// recoveryKeySize is the length of the generated recovery key (auto-unseal mode).
const recoveryKeySize = 32

// RootGenStatus describes a root-token regeneration attempt.
type RootGenStatus struct {
	Started   bool
	Nonce     string
	Progress  int
	Required  int
	Complete  bool
	RootToken string // set only on the update that completes the attempt
}

// rootGen holds the in-progress attempt state.
type rootGen struct {
	nonce    string
	progress [][]byte
}

// RekeyStatus describes a rekey (unseal-share rotation) attempt.
type RekeyStatus struct {
	Started      bool
	Nonce        string
	Progress     int
	Required     int // current-share quorum needed to authorize
	NewShares    int
	NewThreshold int
	Complete     bool
	Keys         [][]byte // the new unseal shares, set only on completion
}

// rekeyAttempt holds the in-progress rekey state, including the target share
// configuration captured at init.
type rekeyAttempt struct {
	nonce        string
	newShares    int
	newThreshold int
	progress     [][]byte
}

// InitConfig parameterizes [Core.Initialize].
type InitConfig struct {
	SecretShares    int // total shares to generate (2..255)
	SecretThreshold int // shares required to unseal (2..SecretShares)
}

// InitResult is returned by [Core.Initialize]. In Shamir mode Keys holds the
// unseal shares. In auto-unseal mode Keys is empty and RecoveryKeys holds the
// recovery shares (which authorize root-token regeneration, not unseal).
// RootToken is the initial root token. All are shown to the operator once.
type InitResult struct {
	Keys         [][]byte
	RecoveryKeys [][]byte
	RootToken    string
}

// SealStatus describes the current lifecycle state.
type SealStatus struct {
	Initialized bool
	Sealed      bool
	Type        string // "shamir" or "auto"
	Shares      int
	Threshold   int
	Progress    int // shares supplied so far toward the current unseal
}

// sealConfig is the persisted seal configuration. In Shamir mode Shares and
// Threshold describe the unseal shares. In auto-unseal mode the master key is
// wrapped by the KEK instead, and these describe the *recovery* shares:
// RecoveryHash is the SHA-256 of a random recovery key that k-of-n recovery
// shares reconstruct, used to authorize root-token regeneration.
type sealConfig struct {
	Type         string `json:"type"`
	Shares       int    `json:"shares"`
	Threshold    int    `json:"threshold"`
	RecoveryHash []byte `json:"recovery_hash,omitempty"`
}

// Core manages initialization and seal/unseal over a storage backend.
type Core struct {
	phys    storage.Backend
	barrier *barrier.Barrier
	tokens  *token.Store
	seal    seal.Seal // auto-unseal seal; nil means Shamir mode

	mu          sync.Mutex
	progress    [][]byte      // unseal shares gathered so far (in-memory only)
	rootAttempt *rootGen      // in-progress root regeneration, if any
	rekey       *rekeyAttempt // in-progress rekey, if any
	sealHooks   []func()      // run by Seal, outside mu
	// stateCh is closed and replaced on every seal and unseal, so a waiter can
	// notice the transition (see stateChanged). Guarded by mu.
	stateCh chan struct{}

	ha ha // active/standby state; zero value when HA is off (ADR D-021)
}

// Option configures a Core.
type Option func(*Core)

// WithSeal enables auto-unseal, using s to wrap and unwrap the master key
// instead of Shamir shares.
func WithSeal(s seal.Seal) Option {
	return func(c *Core) { c.seal = s }
}

// WithAutoUnsealKey enables auto-unseal with a locally-held 32-byte KEK — a
// convenience for WithSeal(seal.NewStaticKEK(kek)).
func WithAutoUnsealKey(kek []byte) Option {
	return WithSeal(seal.NewStaticKEK(kek))
}

// New returns a Core over phys.
func New(phys storage.Backend, opts ...Option) *Core {
	b := barrier.New(phys)
	c := &Core{phys: phys, barrier: b, tokens: token.NewStore(b), stateCh: make(chan struct{})}
	for _, opt := range opts {
		opt(c)
	}
	c.ha.init()
	return c
}

// AutoUnsealEnabled reports whether the core is configured for auto-unseal.
func (c *Core) AutoUnsealEnabled() bool { return c.seal != nil }

// Barrier returns the underlying barrier, for use by upper layers once unsealed.
func (c *Core) Barrier() *barrier.Barrier { return c.barrier }

// Snapshot streams a consistent copy of the entire encrypted store to w. The
// values are ciphertext, so the snapshot never contains plaintext; a restored
// copy still requires the unseal shares (or the auto-unseal KEK).
func (c *Core) Snapshot(ctx context.Context, w io.Writer) error {
	return snapshot.Write(ctx, c.phys, w)
}

// Tokens returns the token store, for authentication by upper layers.
func (c *Core) Tokens() *token.Store { return c.tokens }

// Initialized reports whether the vault has been initialized.
func (c *Core) Initialized(ctx context.Context) (bool, error) {
	return c.barrier.Initialized(ctx)
}

// Initialize sets up a new vault: it generates a master key, initializes the
// barrier with it, splits it into cfg.SecretShares Shamir shares (cfg.Secret-
// Threshold required to reconstruct), persists the share configuration, and
// returns the shares. The vault is left sealed; callers must Unseal.
func (c *Core) Initialize(ctx context.Context, cfg InitConfig) (*InitResult, error) {
	// In auto-unseal mode the shares configure the *recovery* keys; default to
	// 5-of-3 when unspecified so callers that don't care still get recovery keys.
	if c.seal != nil && cfg.SecretShares == 0 && cfg.SecretThreshold == 0 {
		cfg.SecretShares, cfg.SecretThreshold = 5, 3
	}
	// Both modes need a valid k-of-n configuration (unseal shares for Shamir,
	// recovery shares for auto-unseal).
	if cfg.SecretShares < 2 || cfg.SecretShares > 255 ||
		cfg.SecretThreshold < 2 || cfg.SecretThreshold > cfg.SecretShares {
		return nil, ErrInvalidConfig
	}

	// With HA, only the lock holder may write, so initialization takes the lock
	// first (before c.mu: acquiring can wait on another replica). That also
	// serializes replicas initializing the same database concurrently: the
	// loser finds it initialized below.
	if c.ha.enabled() {
		if ok, err := c.barrier.Initialized(ctx); err != nil {
			return nil, err
		} else if ok {
			return nil, ErrAlreadyInitialized
		}
		release, err := c.ha.acquireForInit(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	initialized, err := c.barrier.Initialized(ctx)
	if err != nil {
		return nil, err
	}
	if initialized {
		return nil, ErrAlreadyInitialized
	}

	masterKey := make([]byte, masterKeySize)
	if _, err := rand.Read(masterKey); err != nil {
		return nil, fmt.Errorf("core: generate master key: %w", err)
	}
	defer zero(masterKey)

	if err := c.barrier.Initialize(ctx, masterKey); err != nil {
		return nil, fmt.Errorf("core: initialize barrier: %w", err)
	}

	// Unseal to persist the initial root token. In Shamir mode we re-seal after
	// (the operator must unseal with the shares); in auto mode we leave it
	// unsealed, since the master key is recoverable from the KEK.
	if err := c.barrier.Unseal(ctx, masterKey); err != nil {
		return nil, fmt.Errorf("core: unseal for init: %w", err)
	}
	root, err := c.tokens.CreateRoot(ctx)
	if err != nil {
		c.barrier.Seal()
		return nil, fmt.Errorf("core: create root token: %w", err)
	}

	if c.seal != nil {
		// Auto-unseal: wrap the master key with the seal and store it, leaving
		// the barrier unsealed.
		wrapped, err := c.seal.Wrap(ctx, masterKey)
		if err != nil {
			c.barrier.Seal()
			return nil, err
		}
		if err := c.phys.Put(ctx, &storage.Entry{Key: wrappedMasterKeyPath, Value: wrapped}); err != nil {
			c.barrier.Seal()
			return nil, fmt.Errorf("core: persist wrapped master key: %w", err)
		}

		// Generate recovery keys: a random recovery key split into k-of-n shares,
		// with only its hash persisted. The KEK unseals; the recovery shares
		// authorize root-token regeneration (the sole recovery path when the
		// operator holds no unseal shares).
		recoveryKey := make([]byte, recoveryKeySize)
		if _, err := rand.Read(recoveryKey); err != nil {
			c.barrier.Seal()
			return nil, fmt.Errorf("core: generate recovery key: %w", err)
		}
		defer zero(recoveryKey)
		recoveryShares, err := shamir.Split(recoveryKey, cfg.SecretShares, cfg.SecretThreshold)
		if err != nil {
			c.barrier.Seal()
			return nil, fmt.Errorf("core: split recovery key: %w", err)
		}
		hash := sha256.Sum256(recoveryKey)
		if err := c.writeSealConfig(ctx, sealConfig{
			Type:         c.seal.Type(),
			Shares:       cfg.SecretShares,
			Threshold:    cfg.SecretThreshold,
			RecoveryHash: hash[:],
		}); err != nil {
			return nil, err
		}
		c.notifyLocked() // left unsealed
		return &InitResult{RootToken: root.ID, RecoveryKeys: recoveryShares}, nil
	}

	// Shamir mode: re-seal, split the master key, and return the shares.
	c.barrier.Seal()
	shares, err := shamir.Split(masterKey, cfg.SecretShares, cfg.SecretThreshold)
	if err != nil {
		return nil, fmt.Errorf("core: split master key: %w", err)
	}
	if err := c.writeSealConfig(ctx, sealConfig{Type: SealTypeShamir, Shares: cfg.SecretShares, Threshold: cfg.SecretThreshold}); err != nil {
		return nil, err
	}
	return &InitResult{Keys: shares, RootToken: root.ID}, nil
}

// AutoUnseal unseals the barrier using the configured KEK, without operator
// interaction. It is a no-op if already unsealed.
func (c *Core) AutoUnseal(ctx context.Context) error {
	if c.seal == nil {
		return ErrAutoUnsealNotConfigured
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.barrier.Sealed() {
		return nil
	}
	cfg, err := c.readSealConfig(ctx)
	if err != nil {
		return err
	}
	if !isAutoSeal(cfg.Type) {
		return ErrNotAutoUnseal
	}

	entry, err := c.phys.Get(ctx, wrappedMasterKeyPath)
	if err != nil {
		return fmt.Errorf("core: read wrapped master key: %w", err)
	}
	if entry == nil {
		return fmt.Errorf("core: wrapped master key missing")
	}
	masterKey, err := c.seal.Unwrap(ctx, entry.Value)
	if err != nil {
		return err
	}
	defer zero(masterKey)

	if err := c.barrier.Unseal(ctx, masterKey); err != nil {
		return fmt.Errorf("core: auto-unseal barrier: %w", err)
	}
	c.notifyLocked()
	return nil
}

// Unseal supplies one unseal share. It returns the resulting status. When the
// number of distinct shares reaches the threshold, it reconstructs the master
// key and unseals the barrier; if the shares do not reconstruct a valid key, the
// gathered progress is discarded and [ErrUnsealFailed] is returned.
func (c *Core) Unseal(ctx context.Context, share []byte) (*SealStatus, error) {
	if len(share) != masterKeySize+1 {
		return nil, ErrInvalidShare
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	cfg, err := c.readSealConfig(ctx)
	if err != nil {
		return nil, err
	}
	if isAutoSeal(cfg.Type) {
		return nil, ErrAutoUnsealShamir
	}

	if !c.barrier.Sealed() {
		return c.statusLocked(cfg, false), nil // already unsealed
	}

	// Ignore a share that has already been supplied.
	if !containsShare(c.progress, share) {
		c.progress = append(c.progress, append([]byte(nil), share...))
	}

	if len(c.progress) < cfg.Threshold {
		return c.statusLocked(cfg, true), nil
	}

	masterKey, err := shamir.Combine(c.progress)
	if err != nil {
		c.resetProgress()
		return nil, fmt.Errorf("core: combine shares: %w", err)
	}
	defer zero(masterKey)

	if err := c.barrier.Unseal(ctx, masterKey); err != nil {
		// Wrong/inconsistent shares reconstruct the wrong key and fail barrier
		// authentication. Discard progress so the operator can start over.
		c.resetProgress()
		if errors.Is(err, barrier.ErrInvalidKey) {
			return nil, ErrUnsealFailed
		}
		return nil, fmt.Errorf("core: unseal barrier: %w", err)
	}

	c.resetProgress()
	c.notifyLocked()
	return c.statusLocked(cfg, false), nil
}

// GenerateRootInit starts a root-token regeneration attempt and returns its
// nonce. Providing a quorum of unseal shares to GenerateRootUpdate then mints a
// new root token — the recovery path for a lost root token (docs/DESIGN.md §8.3).
// Only Shamir-unseal vaults are supported, and the vault must be unsealed (so the
// new token can be persisted).
func (c *Core) GenerateRootInit(ctx context.Context) (*RootGenStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cfg, err := c.readSealConfig(ctx)
	if err != nil {
		return nil, err
	}
	// Shamir vaults verify against the master key; auto-unseal vaults verify
	// against the recovery keys generated at init.
	if isAutoSeal(cfg.Type) && len(cfg.RecoveryHash) == 0 {
		return nil, ErrRootGenNoRecovery
	}
	if c.barrier.Sealed() {
		return nil, ErrRootGenSealed
	}

	nonce, err := randomNonce()
	if err != nil {
		return nil, err
	}
	c.rootAttempt = &rootGen{nonce: nonce}
	return &RootGenStatus{Started: true, Nonce: nonce, Progress: 0, Required: cfg.Threshold}, nil
}

// GenerateRootUpdate supplies one unseal share to the attempt identified by
// nonce. When the threshold is reached it reconstructs the master key, verifies
// it, and (if valid) returns a freshly minted root token in the status.
func (c *Core) GenerateRootUpdate(ctx context.Context, nonce string, share []byte) (*RootGenStatus, error) {
	if len(share) != masterKeySize+1 {
		return nil, ErrInvalidShare
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.rootAttempt == nil {
		return nil, ErrRootGenNotStarted
	}
	if nonce != c.rootAttempt.nonce {
		return nil, ErrRootGenNonce
	}
	cfg, err := c.readSealConfig(ctx)
	if err != nil {
		return nil, err
	}

	if !containsShare(c.rootAttempt.progress, share) {
		c.rootAttempt.progress = append(c.rootAttempt.progress, append([]byte(nil), share...))
	}
	if len(c.rootAttempt.progress) < cfg.Threshold {
		return &RootGenStatus{Started: true, Nonce: nonce, Progress: len(c.rootAttempt.progress), Required: cfg.Threshold}, nil
	}

	combined, err := shamir.Combine(c.rootAttempt.progress)
	if err != nil {
		c.rootAttempt = nil
		return nil, fmt.Errorf("core: combine shares: %w", err)
	}
	defer zero(combined)

	// Shamir: the combined value is the master key, verified against the barrier.
	// Auto-unseal: it is the recovery key, verified against its stored hash.
	var ok bool
	if isAutoSeal(cfg.Type) {
		got := sha256.Sum256(combined)
		ok = subtle.ConstantTimeCompare(got[:], cfg.RecoveryHash) == 1
	} else {
		ok, err = c.barrier.VerifyMasterKey(ctx, combined)
		if err != nil {
			c.rootAttempt = nil
			return nil, err
		}
	}
	if !ok {
		c.rootAttempt = nil
		return nil, ErrUnsealFailed
	}

	root, err := c.tokens.CreateRoot(ctx)
	if err != nil {
		c.rootAttempt = nil
		return nil, fmt.Errorf("core: create root token: %w", err)
	}
	c.rootAttempt = nil
	return &RootGenStatus{Started: false, Complete: true, Required: cfg.Threshold, RootToken: root.ID}, nil
}

// GenerateRootCancel discards any in-progress attempt.
func (c *Core) GenerateRootCancel() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rootAttempt = nil
}

// GenerateRootStatus reports the current attempt.
func (c *Core) GenerateRootStatus(ctx context.Context) (*RootGenStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.rootAttempt == nil {
		return &RootGenStatus{Started: false}, nil
	}
	cfg, err := c.readSealConfig(ctx)
	if err != nil {
		return nil, err
	}
	return &RootGenStatus{
		Started:  true,
		Nonce:    c.rootAttempt.nonce,
		Progress: len(c.rootAttempt.progress),
		Required: cfg.Threshold,
	}, nil
}

// RekeyInit starts a rekey attempt that will re-split the master key into
// newShares Shamir shares (newThreshold required), rotating the unseal shares —
// for example when a share-holder leaves. It returns a nonce; feeding a quorum of
// the *current* shares to RekeyUpdate then rewraps the master key and returns the
// new shares. Only Shamir-unseal vaults are supported, and the vault must be
// unsealed. The barrier key and all data are untouched, so the vault keeps
// serving throughout.
func (c *Core) RekeyInit(ctx context.Context, newShares, newThreshold int) (*RekeyStatus, error) {
	if newShares < 2 || newShares > 255 || newThreshold < 2 || newThreshold > newShares {
		return nil, ErrInvalidConfig
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	cfg, err := c.readSealConfig(ctx)
	if err != nil {
		return nil, err
	}
	if isAutoSeal(cfg.Type) {
		return nil, ErrRekeyNotShamir
	}
	if c.barrier.Sealed() {
		return nil, ErrRekeySealed
	}

	nonce, err := randomNonce()
	if err != nil {
		return nil, err
	}
	c.rekey = &rekeyAttempt{nonce: nonce, newShares: newShares, newThreshold: newThreshold}
	return &RekeyStatus{
		Started:      true,
		Nonce:        nonce,
		Progress:     0,
		Required:     cfg.Threshold,
		NewShares:    newShares,
		NewThreshold: newThreshold,
	}, nil
}

// RekeyUpdate supplies one current unseal share to the attempt identified by
// nonce. When the current threshold is reached it reconstructs and verifies the
// master key, generates a new one, rewraps the barrier keyring under it, persists
// the new share configuration, and returns the freshly split unseal shares. Those
// shares are shown once — the old shares stop working once this completes.
func (c *Core) RekeyUpdate(ctx context.Context, nonce string, share []byte) (*RekeyStatus, error) {
	if len(share) != masterKeySize+1 {
		return nil, ErrInvalidShare
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.rekey == nil {
		return nil, ErrRekeyNotStarted
	}
	if nonce != c.rekey.nonce {
		return nil, ErrRekeyNonce
	}
	cfg, err := c.readSealConfig(ctx)
	if err != nil {
		return nil, err
	}
	if c.barrier.Sealed() {
		return nil, ErrRekeySealed
	}

	if !containsShare(c.rekey.progress, share) {
		c.rekey.progress = append(c.rekey.progress, append([]byte(nil), share...))
	}
	if len(c.rekey.progress) < cfg.Threshold {
		return &RekeyStatus{
			Started:      true,
			Nonce:        nonce,
			Progress:     len(c.rekey.progress),
			Required:     cfg.Threshold,
			NewShares:    c.rekey.newShares,
			NewThreshold: c.rekey.newThreshold,
		}, nil
	}

	oldMasterKey, err := shamir.Combine(c.rekey.progress)
	if err != nil {
		c.rekey = nil
		return nil, fmt.Errorf("core: combine shares: %w", err)
	}
	defer zero(oldMasterKey)

	ok, err := c.barrier.VerifyMasterKey(ctx, oldMasterKey)
	if err != nil {
		c.rekey = nil
		return nil, err
	}
	if !ok {
		c.rekey = nil
		return nil, ErrUnsealFailed
	}

	newMasterKey := make([]byte, masterKeySize)
	if _, err := rand.Read(newMasterKey); err != nil {
		c.rekey = nil
		return nil, fmt.Errorf("core: generate master key: %w", err)
	}
	defer zero(newMasterKey)

	// Split the new key first (in memory) so a failure here changes nothing on
	// disk. Only then rewrap the keyring and commit the new share configuration.
	newShares := c.rekey.newShares
	newThreshold := c.rekey.newThreshold
	shares, err := shamir.Split(newMasterKey, newShares, newThreshold)
	if err != nil {
		c.rekey = nil
		return nil, fmt.Errorf("core: split master key: %w", err)
	}
	if err := c.barrier.Rekey(ctx, oldMasterKey, newMasterKey); err != nil {
		c.rekey = nil
		return nil, fmt.Errorf("core: rewrap keyring: %w", err)
	}
	// The keyring now requires newMasterKey; record the matching share config.
	if err := c.writeSealConfig(ctx, sealConfig{Type: SealTypeShamir, Shares: newShares, Threshold: newThreshold}); err != nil {
		// The keyring is already rewrapped, so the new shares are the only way in:
		// return them alongside the error rather than stranding the operator.
		c.rekey = nil
		return &RekeyStatus{Complete: true, NewShares: newShares, NewThreshold: newThreshold, Keys: shares}, err
	}

	c.rekey = nil
	return &RekeyStatus{
		Started:      false,
		Complete:     true,
		Required:     cfg.Threshold,
		NewShares:    newShares,
		NewThreshold: newThreshold,
		Keys:         shares,
	}, nil
}

// RekeyCancel discards any in-progress rekey attempt.
func (c *Core) RekeyCancel() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rekey = nil
}

// RekeyStatus reports the current rekey attempt.
func (c *Core) RekeyStatus(ctx context.Context) (*RekeyStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.rekey == nil {
		return &RekeyStatus{Started: false}, nil
	}
	cfg, err := c.readSealConfig(ctx)
	if err != nil {
		return nil, err
	}
	return &RekeyStatus{
		Started:      true,
		Nonce:        c.rekey.nonce,
		Progress:     len(c.rekey.progress),
		Required:     cfg.Threshold,
		NewShares:    c.rekey.newShares,
		NewThreshold: c.rekey.newThreshold,
	}, nil
}

// Seal re-seals the barrier, discards any in-progress unseal shares, and then
// runs the hooks registered with [Core.OnSeal].
func (c *Core) Seal() {
	c.mu.Lock()
	c.barrier.Seal()
	c.resetProgress()
	c.notifyLocked()
	hooks := slices.Clone(c.sealHooks)
	c.mu.Unlock()

	for _, fn := range hooks {
		fn()
	}
}

// OnSeal registers fn to run each time [Core.Seal] seals the vault, after the
// barrier is sealed and outside the core's lock. Upper layers use it to drop
// state they cached from the barrier, so the next unseal starts from what
// storage holds then rather than from what this process saw before.
func (c *Core) OnSeal(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sealHooks = append(c.sealHooks, fn)
}

// Status returns the current seal status.
func (c *Core) Status(ctx context.Context) (*SealStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cfg, err := c.readSealConfig(ctx)
	if errors.Is(err, ErrNotInitialized) {
		return &SealStatus{Initialized: false, Sealed: true}, nil
	}
	if err != nil {
		return nil, err
	}
	return c.statusLocked(cfg, c.barrier.Sealed()), nil
}

// statusLocked builds a SealStatus. The caller must hold c.mu.
func (c *Core) statusLocked(cfg *sealConfig, sealed bool) *SealStatus {
	return &SealStatus{
		Initialized: true,
		Sealed:      sealed,
		Type:        cfg.Type,
		Shares:      cfg.Shares,
		Threshold:   cfg.Threshold,
		Progress:    len(c.progress),
	}
}

func (c *Core) resetProgress() {
	for _, s := range c.progress {
		zero(s)
	}
	c.progress = nil
}

func (c *Core) writeSealConfig(ctx context.Context, cfg sealConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("core: marshal seal config: %w", err)
	}
	if err := c.phys.Put(ctx, &storage.Entry{Key: sealConfigPath, Value: data}); err != nil {
		return fmt.Errorf("core: persist seal config: %w", err)
	}
	return nil
}

func (c *Core) readSealConfig(ctx context.Context) (*sealConfig, error) {
	entry, err := c.phys.Get(ctx, sealConfigPath)
	if err != nil {
		return nil, fmt.Errorf("core: read seal config: %w", err)
	}
	if entry == nil {
		return nil, ErrNotInitialized
	}
	var cfg sealConfig
	if err := json.Unmarshal(entry.Value, &cfg); err != nil {
		return nil, fmt.Errorf("core: parse seal config: %w", err)
	}
	if cfg.Type == "" {
		cfg.Type = SealTypeShamir // configs written before seal types existed
	}
	return &cfg, nil
}

func containsShare(list [][]byte, s []byte) bool {
	for _, x := range list {
		if bytes.Equal(x, s) {
			return true
		}
	}
	return false
}

// randomNonce returns a random hex nonce for a root-generation attempt.
func randomNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("core: generate nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// zero overwrites b with zeros (best-effort key hygiene).
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

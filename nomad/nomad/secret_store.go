// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/nomad/seal"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/secretstore"
	"github.com/hashicorp/nomad/secretstore/ubix/export/core"
	"github.com/hashicorp/nomad/secretstore/ubix/export/storage"
)

// secretCoreKeyringPath is where the barrier of the core keeps its key,
// encrypted with the core's master key.
const secretCoreKeyringPath = "core/keyring"

// secretCoreActiveTimeout bounds how long secretCore waits on the leader for
// the core to become active (it takes the HA lock right after leadership
// is established or the core is unsealed).
const secretCoreActiveTimeout = 5 * time.Second

// setupSecretStore creates the storage and HA lock backend of the built-in
// secret store on this server's Raft (see nomad/secretstore) and the
// uBixVault core over it.
//
// The core's lifecycle (Norca phase 1, the seal):
//
//   - Every server creates the core with HA over the backend; its holder ID
//     is the server's Raft ID, and the HA lock is Nomad leadership, so only
//     the leader's core becomes active. RunHA runs until shutdown.
//   - The core runs in uBixVault's auto-unseal mode with seal.CoreSeal: its
//     master key is wrapped by the seals of the server's keyring, so the
//     unseal of the server (automatic with aead, shares with the shares
//     seal) also unseals the core. Every server unseals its own core
//     (runSecretCoreUnseal) as soon as its seal is available and the core is
//     initialized.
//   - The leader initializes the core the first time the store is used
//     (secretCore). uBixVault's Initialize mints a root token and recovery
//     shares; Norca uses neither and drops them.
func (s *Server) setupSecretStore() {
	s.secretStore = secretstore.New(&secretstore.Deps{
		Logger: s.logger.Named("secret_store"),
		State:  func() *state.StateStore { return s.fsm.State() },
		Apply: func(msgType structs.MessageType, req any) (uint64, error) {
			_, index, err := s.raftApply(msgType, req)
			return index, err
		},
		ServersReady: func() bool {
			return ServersMeetMinimumNorcaVersion(s.Members(), s.Region(), false)
		},
		Leader: func() (string, string) {
			addr, id := s.raft.LeaderWithID()
			return string(id), string(addr)
		},
		Term: s.raftTerm,
	})

	s.secretStoreCore = core.New(s.secretStore,
		core.WithSeal(&seal.CoreSeal{Keyring: s.seal}),
		core.WithHA(core.HAConfig{
			Backend:  s.secretStore,
			HolderID: string(s.config.RaftConfig.LocalID),
		}))
	logger := s.logger.Named("secret_store")
	go func() {
		err := s.secretStoreCore.RunHA(s.shutdownCtx, func(format string, args ...any) {
			logger.Debug(fmt.Sprintf(format, args...))
		})
		if err != nil {
			logger.Error("secret store core stopped", "error", err)
		}
	}()
	go s.runSecretCoreUnseal(s.shutdownCtx)
}

// raftTerm returns the current Raft term of this server.
func (s *Server) raftTerm() uint64 {
	term, _ := strconv.ParseUint(s.raft.Stats()["term"], 10, 64)
	return term
}

// secretCore returns the active, unsealed core of the built-in secret store.
// It is active on the leader only; followers forward secret requests to the
// leader. On the leader it initializes the core the first time it is used.
// It returns an error wrapping structs.ErrSecretStoreSealed while the server
// or the core is sealed, the core is not initialized, or it is not active
// yet.
func (s *Server) secretCore() (*core.Core, error) {
	c := s.secretStoreCore
	if c == nil {
		return nil, fmt.Errorf("%w: the secret store is not set up", structs.ErrSecretStoreSealed)
	}
	if c.Active() {
		return c, nil
	}
	if s.seal.sealed() {
		return nil, fmt.Errorf("%w: unseal the server with 'norca operator unseal'", structs.ErrSecretStoreSealed)
	}
	if s.secretCoreNeedsRecovery() {
		return nil, fmt.Errorf("%w: the restored secret store is wrapped by seals this region does not use; "+
			"recover it with the old shares ('norca operator unseal -recover')", structs.ErrSecretStoreSealed)
	}
	if !s.IsLeader() {
		return nil, fmt.Errorf("%w: the secret store core is active on the leader only", structs.ErrSecretStoreSealed)
	}

	ctx, cancel := context.WithTimeout(s.shutdownCtx, secretCoreActiveTimeout)
	defer cancel()
	if err := s.initSecretCore(ctx); err != nil {
		return nil, err
	}
	for !c.Active() {
		select {
		case <-ctx.Done():
			if c.Barrier().Sealed() {
				return nil, fmt.Errorf("%w: the secret store core is sealed", structs.ErrSecretStoreSealed)
			}
			return nil, fmt.Errorf("%w: the secret store core is not active yet", structs.ErrSecretStoreSealed)
		case <-time.After(10 * time.Millisecond):
		}
	}
	return c, nil
}

// initSecretCore initializes the core on the leader if it is not, leaving it
// unsealed. It is a no-op on an initialized core.
func (s *Server) initSecretCore(ctx context.Context) error {
	c := s.secretStoreCore
	ok, err := c.Initialized(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", structs.ErrSecretStoreSealed, err)
	}
	if ok {
		return nil
	}
	if _, err := s.seal.wrapSeals(); err != nil {
		return fmt.Errorf("%w: %v", structs.ErrSecretStoreSealed, err)
	}
	if !ServersMeetMinimumNorcaVersion(s.Members(), s.Region(), false) {
		return fmt.Errorf("%w: not initialized: %v", structs.ErrSecretStoreSealed, secretstore.ErrServersNotReady)
	}
	res, err := c.Initialize(ctx, core.InitConfig{})
	if errors.Is(err, core.ErrAlreadyInitialized) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: failed to initialize the secret store: %v", structs.ErrSecretStoreSealed, err)
	}
	// Norca authorizes with its own ACLs and never uses the core's root
	// token or recovery shares.
	for _, k := range res.RecoveryKeys {
		clear(k)
	}
	s.logger.Named("secret_store").Info("initialized the secret store core")
	return nil
}

// runSecretCoreUnseal unseals the core of this server whenever it is sealed,
// initialized, and the server's seal is available, until ctx ends. If the
// stored barrier keyring changes underneath an unsealed core (a snapshot or
// backup restore), the core is sealed and unsealed again from the restored
// state.
func (s *Server) runSecretCoreUnseal(ctx context.Context) {
	logger := s.logger.Named("secret_store")
	c := s.secretStoreCore
	var unsealedKeyring []byte
	lastErr := ""
	for {
		store := s.fsm.State()
		ws := memdb.NewWatchSet()
		ws.Add(store.AbandonCh())
		ws.Add(s.seal.changedCh())
		keyring, err1 := store.SecretStoreBlobByPath(ws, secretCoreKeyringPath)
		master, err2 := store.SecretStoreBlobByPath(ws, seal.CoreMasterKeyPath)
		wait := 30 * time.Second
		if err := errors.Join(err1, err2); err != nil {
			logger.Error("failed to read the secret store state", "error", err)
			wait = 2 * time.Second
		} else {
			if !c.Barrier().Sealed() && unsealedKeyring != nil &&
				(keyring == nil || !bytes.Equal(keyring.Value, unsealedKeyring)) {
				logger.Info("the secret store was replaced by a restore; unsealing its core again")
				c.Seal()
				unsealedKeyring = nil
			}
			// Norca: a restored core key wrapped by seals the region does not
			// use keeps the store sealed until it is recovered, even if the
			// core runs with the same barrier key
			recovery := master != nil && s.coreValueNeedsRecovery(master.Value)
			if recovery && !c.Barrier().Sealed() {
				logger.Warn("the restored secret store is wrapped by seals this region does not use; " +
					"it is sealed until it is recovered with the old shares ('norca operator unseal -recover')")
				c.Seal()
				unsealedKeyring = nil
			}
			switch {
			case !c.Barrier().Sealed():
				if unsealedKeyring == nil && keyring != nil {
					// unsealed by Initialize on this server
					unsealedKeyring = slices.Clone(keyring.Value)
				}
			case master == nil || keyring == nil || s.seal.sealed() || recovery:
				// not initialized, or not with Norca's seal, or sealed
			default:
				err := c.AutoUnseal(ctx)
				switch {
				case err == nil:
					unsealedKeyring = slices.Clone(keyring.Value)
					lastErr = ""
					logger.Info("secret store core unsealed")
				case ctx.Err() != nil:
					return
				default:
					if err.Error() != lastErr {
						logger.Warn("failed to unseal the secret store core", "error", err)
					}
					lastErr = err.Error()
					wait = 5 * time.Second
				}
			}
		}
		wctx, cancel := context.WithTimeout(ctx, wait)
		_ = ws.WatchCtx(wctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
	}
}

// rewrapSecretCoreKey wraps the stored master key of the core with the
// seals keys are wrapped with now, on the leader. It does nothing if the
// core is not initialized with Norca's seal or already wrapped that way.
// During a rewrap (add) it adds the wraps of the new seals; otherwise it
// never adds an aead wrap, like rewrapKeystore. A key wrapped by no seal this
// server holds (a restored store that needs recovery) is left alone.
func (s *Server) rewrapSecretCoreKey(ctx context.Context, add bool) error {
	blob, err := s.State().SecretStoreBlobByPath(nil, seal.CoreMasterKeyPath)
	if err != nil || blob == nil {
		return err
	}
	env, err := seal.ParseCoreEnvelope(blob.Value)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(env.Wraps, s.seal.canUnwrap) {
		return nil
	}
	seals, err := s.seal.wrapSeals()
	if err != nil {
		return err
	}
	have := make([]string, 0, len(env.Wraps))
	for _, w := range env.Wraps {
		have = append(have, sealWrapTag(w.Seal, w.KeyID))
	}
	slices.Sort(have)
	if !add {
		seals = keepOrStrengthen(seals, have)
		if len(seals) == 0 {
			return nil
		}
	}
	want := sealWrapTags(seals)
	if slices.Equal(have, want) {
		return nil
	}
	value, err := seal.RewrapCoreKey(ctx, s.seal, env, seals)
	if err != nil {
		return err
	}
	if err := s.putSecretStoreBlob(ctx, seal.CoreMasterKeyPath, value); err != nil {
		return fmt.Errorf("failed to store the rewrapped secret store key: %w", err)
	}
	s.logger.Named("secret_store").Info("rewrapped the secret store key", "seals", want)
	return nil
}

// putSecretStoreBlob writes a value of the core's storage outside the core,
// on the leader: directly while the core holds the HA lock, otherwise under
// a lock handle of its own.
func (s *Server) putSecretStoreBlob(ctx context.Context, key string, value []byte) error {
	entry := &storage.Entry{Key: key, Value: value}
	err := s.secretStore.Put(ctx, entry)
	if !errors.Is(err, storage.ErrFenced) {
		return err
	}
	lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	l := s.secretStore.HALock("active", string(s.config.RaftConfig.LocalID)+"/seal", "", storage.LockOptions{})
	if _, err := l.Acquire(lctx); err != nil {
		return err
	}
	defer l.Release(context.Background())
	return s.secretStore.Put(ctx, entry)
}

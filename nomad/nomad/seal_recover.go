// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/nomad/nomad/seal"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/secretstore"
	"github.com/hashicorp/nomad/secretstore/ubix/export/barrier"
	"github.com/hashicorp/nomad/secretstore/ubix/export/shamir"
)

// Recovery of a restored secret store (norca-doc content/docs/design/secrets-plan.md, "Backup
// and restore"). A full restore brings back the Raft state of a backup, with
// the key of the secret store core wrapped by the seals of the region at the
// time of the backup. If the region has other seals now (other shares), the
// core can't unseal: the store stays sealed (ServerSealStatus.
// SecretCoreRecovery) until its key is recovered and re-wrapped with the
// region's seals, either from the secret store key object of the backup
// (the restore does it with the age identity) or with the old shares that
// an operator enters on the leader (Operator.SealRecover).

// maxRecoveryShares bounds the old shares collected toward a recovery.
const maxRecoveryShares = 255

// secretCoreEnvelope returns the stored, wrapped key of the secret store
// core, or nil if the core is not initialized with Norca's seal.
func (s *Server) secretCoreEnvelope() *seal.CoreEnvelope {
	if s.fsm == nil {
		return nil
	}
	return storedCoreEnvelope(s.fsm.State())
}

// storedCoreEnvelope returns the wrapped key of the secret store core in a
// state store, or nil.
func storedCoreEnvelope(store *state.StateStore) *seal.CoreEnvelope {
	blob, err := store.SecretStoreBlobByPath(nil, seal.CoreMasterKeyPath)
	if err != nil || blob == nil {
		return nil
	}
	env, err := seal.ParseCoreEnvelope(blob.Value)
	if err != nil {
		return nil
	}
	return env
}

// secretCoreNeedsRecovery reports whether the stored key of the secret store
// core is wrapped by none of the seals of the region.
func (s *Server) secretCoreNeedsRecovery() bool {
	seals, rewrap := s.seal.current()
	return coreNeedsRecovery(s.secretCoreEnvelope(), seals, rewrap)
}

// coreValueNeedsRecovery reports whether a stored core key (the value of
// seal.CoreMasterKeyPath) is wrapped by none of the seals of the region.
func (s *Server) coreValueNeedsRecovery(value []byte) bool {
	env, err := seal.ParseCoreEnvelope(value)
	if err != nil {
		return false
	}
	seals, rewrap := s.seal.current()
	return coreNeedsRecovery(env, seals, rewrap)
}

// recoveryProgress returns the number of old shares entered on this server.
func (m *sealManager) recoveryProgress() int {
	m.recoveryLock.Lock()
	defer m.recoveryLock.Unlock()
	return len(m.recovery)
}

func (m *sealManager) resetRecovery() {
	m.recoveryLock.Lock()
	defer m.recoveryLock.Unlock()
	for _, p := range m.recovery {
		clear(p)
	}
	m.recovery = nil
}

// addRecoveryShare adds an old share and returns the key of the secret
// store core once the shares entered reconstruct an unseal key that unwraps
// it, or nil. The shares are kept until then: a backup records no
// threshold the core key can be checked against, so every combination of
// two or more shares is tried.
func (m *sealManager) addRecoveryShare(env *seal.CoreEnvelope, share []byte) ([]byte, int) {
	m.recoveryLock.Lock()
	defer m.recoveryLock.Unlock()
	if !slices.ContainsFunc(m.recovery, func(p []byte) bool { return bytes.Equal(p, share) }) {
		if len(m.recovery) >= maxRecoveryShares {
			return nil, len(m.recovery)
		}
		m.recovery = append(m.recovery, slices.Clone(share))
	}
	if len(m.recovery) < 2 {
		return nil, len(m.recovery)
	}
	unsealKey, err := shamir.Combine(m.recovery)
	if err != nil {
		return nil, len(m.recovery)
	}
	defer clear(unsealKey)
	for _, w := range env.Wraps {
		if w == nil || w.Seal != structs.SealTypeShares {
			continue
		}
		if key, err := seal.UnwrapCoreKeyWith(unsealKey, w); err == nil {
			for _, p := range m.recovery {
				clear(p)
			}
			m.recovery = nil
			return key, 0
		}
	}
	return nil, len(m.recovery)
}

// verifySecretCoreKey checks that a key unlocks the barrier keyring of the
// secret store core in a state store, without touching the running core.
func verifySecretCoreKey(ctx context.Context, store *state.StateStore, key []byte) error {
	b := barrier.New(secretstore.New(&secretstore.Deps{
		State: func() *state.StateStore { return store },
	}))
	if err := b.Unseal(ctx, key); err != nil {
		return fmt.Errorf("the key does not unlock the secret store: %w", err)
	}
	b.Seal()
	return nil
}

// recoverSecretCoreWithKey re-wraps the key of a restored secret store core
// with the seals of the region, on the leader. It does nothing if the core
// needs no recovery.
func (s *Server) recoverSecretCoreWithKey(ctx context.Context, key []byte) error {
	if !s.secretCoreNeedsRecovery() {
		return nil
	}
	if err := verifySecretCoreKey(ctx, s.State(), key); err != nil {
		return err
	}
	seals, err := s.seal.wrapSeals()
	if err != nil {
		return err
	}
	value, err := seal.WrapCoreKey(ctx, seals, key)
	if err != nil {
		return err
	}
	if err := s.putSecretStoreBlob(ctx, seal.CoreMasterKeyPath, value); err != nil {
		return fmt.Errorf("failed to store the recovered secret store key: %w", err)
	}
	s.seal.resetRecovery()
	s.logger.Named("secret_store").Info("recovered the key of the restored secret store and re-wrapped it",
		"audit", true, "seals", sealWrapTags(seals))
	return nil
}

// SealRecover recovers the key of a restored secret store core that is
// wrapped by none of the seals of the region: with one old unseal share per
// request (operator:write), collected on the leader, or with the key itself
// from the secret store key object of a backup (management). The leader
// re-wraps the key with the region's seals, and the core unseals.
func (op *Operator) SealRecover(args *structs.SealRecoverRequest, reply *structs.SealRecoverResponse) error {
	authErr := op.srv.Authenticate(op.ctx, args)
	if done, err := op.srv.forward("Operator.SealRecover", args, args, reply); done {
		return err
	}
	op.srv.MeasureRPCRate("operator", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	aclObj, err := op.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if aclObj != nil && !aclObj.AllowOperatorWrite() {
		return structs.ErrPermissionDenied
	}
	if len(args.CoreKey) > 0 && aclObj != nil && !aclObj.IsManagement() {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "seal", "recover"}, time.Now())

	s := op.srv
	m := s.seal
	defer func() {
		if env := s.secretCoreEnvelope(); env != nil {
			reply.Seals = env.Seals()
		}
		reply.Index, _ = s.State().LatestIndex()
	}()
	env := s.secretCoreEnvelope()
	if env == nil {
		return structs.NewErrRPCCoded(http.StatusBadRequest, "the secret store is not initialized: nothing to recover")
	}
	if !s.secretCoreNeedsRecovery() {
		m.resetRecovery()
		reply.Recovered = true
		return nil
	}
	if m.sealed() {
		return fmt.Errorf("%w: unseal the leader before the recovery", structs.ErrSealed)
	}

	ctx, cancel := context.WithTimeout(s.shutdownCtx, 30*time.Second)
	defer cancel()
	var key []byte
	switch {
	case args.Reset:
		m.resetRecovery()
		return nil
	case len(args.CoreKey) > 0:
		key = slices.Clone(args.CoreKey)
	case args.Key != "":
		share, err := decodeShare(args.Key)
		if err != nil {
			return structs.NewErrRPCCoded(http.StatusBadRequest, err.Error())
		}
		key, reply.Progress = m.addRecoveryShare(env, share)
		clear(share)
		if key == nil {
			if reply.Progress >= maxRecoveryShares {
				return structs.NewErrRPCCodedf(http.StatusBadRequest,
					"the %d shares entered do not unwrap the secret store key; reset and enter the old shares again",
					reply.Progress)
			}
			return nil
		}
	default:
		return structs.NewErrRPCCoded(http.StatusBadRequest, "an old unseal share or the secret store key is required")
	}
	defer clear(key)

	err = s.recoverSecretCoreWithKey(ctx, key)
	switch {
	case err == nil:
		reply.Recovered = true
		return nil
	case structs.IsErrSealed(err):
		return err
	case errors.Is(err, barrier.ErrInvalidKey) || errors.Is(err, barrier.ErrNotInitialized):
		return structs.NewErrRPCCoded(http.StatusBadRequest, err.Error())
	}
	return err
}

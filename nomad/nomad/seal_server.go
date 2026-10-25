// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/seal"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/hashicorp/serf/serf"
)

// sealManager is the keyring seal of this server (see nomad/seal for what
// the seal protects and how).
//
// The seals of the region are recorded in Raft (structs.SealConfig); without
// a record the region uses the aead seal, as Nomad did, unless this server is
// configured with a shares seal and the keyring has no key yet: then the
// region is uninitialized until the first rewrap generates the shares.
//
// A server is sealed while none of the region's seals is available to it.
// The aead seal is always available; a shares seal is available once its
// unseal key is reconstructed from shares (Operator.SealUnseal), or, during
// a rewrap, unwrapped from the rewrap record with a current seal.
//
// Key distribution (decided 2026-10-25, the model Vault documents): in a
// region with a seal other than aead, key material never travels between
// servers in usable form. Keyring.Get, the keyring restore fan-out and the
// rewrap carry keys wrapped by the region's seals only, and every server
// unwraps them with its own unsealed seal; a sealed server, or a client with
// a stolen server certificate, gets nothing it can use. With the aead seal
// the keys travel as in Nomad.
type sealManager struct {
	srv    *Server
	logger hclog.Logger

	// cfg is the seal of the agent configuration, with defaults.
	cfg *config.KeyringSealConfig

	aead seal.AEAD

	// unsealer collects the shares entered toward the next unseal; once
	// they reconstruct the unseal key, the key moves to shares.
	unsealer *seal.Shares

	// sealedShares stands for a shares seal whose key this server does not
	// hold: it is never available, so unwrapping with it reports sealed.
	sealedShares *seal.Shares

	// shares are the unsealed shares seals of this server by unseal key ID:
	// one, or two during a rewrap from shares to shares; guarded by
	// keysLock.
	shares   map[string]*seal.Shares
	keysLock sync.Mutex

	// recovery collects the old shares entered on the leader to recover the
	// key of a restored secret store core (Operator.SealRecover); guarded
	// by recoveryLock.
	recovery     [][]byte
	recoveryLock sync.Mutex

	// rewrapLock serializes the start of rewraps on the leader.
	rewrapLock sync.Mutex

	lock sync.Mutex
	// changed is closed and replaced when the seal state of this server
	// changes (unsealed, a seal key added or dropped, a new seal
	// configuration seen).
	changed chan struct{}
	// configIndex is the modify index of the last seal configuration the
	// reconcile loop handled.
	configIndex uint64
	// lastErr is the last error of the reconcile loop, logged once.
	lastErr string
	// reportedSealed is the ID of the last rewrap this server reported it
	// can't take part in.
	reportedSealed string
}

func newSealManager(s *Server, cfg *config.KeyringConfig) *sealManager {
	m := &sealManager{
		srv:          s,
		logger:       s.logger.Named("seal"),
		cfg:          cfg.Seal(),
		unsealer:     seal.NewShares(),
		sealedShares: seal.NewShares(),
		shares:       make(map[string]*seal.Shares),
		changed:      make(chan struct{}),
	}
	if m.cfg.Type == structs.SealTypeAEAD {
		m.logger.Warn("the keyring uses the aead seal: the root keys are stored next to their key-encrypting keys; " +
			`use it for development and migration only and configure server { keyring { seal "shares" } }`)
	}
	return m
}

// changedCh returns a channel closed at the next change of the seal state.
func (m *sealManager) changedCh() <-chan struct{} {
	m.lock.Lock()
	defer m.lock.Unlock()
	return m.changed
}

func (m *sealManager) notify() {
	m.lock.Lock()
	defer m.lock.Unlock()
	close(m.changed)
	m.changed = make(chan struct{})
}

// sharesKey returns the unsealed shares seal of an unseal key, or nil.
func (m *sealManager) sharesKey(keyID string) *seal.Shares {
	m.keysLock.Lock()
	defer m.keysLock.Unlock()
	return m.shares[keyID]
}

// addSharesKey makes a shares seal available with its unseal key.
func (m *sealManager) addSharesKey(info *structs.SealInfo, key []byte) error {
	s := seal.NewShares()
	if err := s.SetKey(info, key); err != nil {
		return err
	}
	m.keysLock.Lock()
	defer m.keysLock.Unlock()
	if old := m.shares[info.KeyID]; old != nil {
		old.Clear()
	}
	m.shares[info.KeyID] = s
	return nil
}

// dropSharesKey forgets the unseal key of a shares seal.
func (m *sealManager) dropSharesKey(keyID string) {
	m.keysLock.Lock()
	defer m.keysLock.Unlock()
	if s := m.shares[keyID]; s != nil {
		s.Clear()
		delete(m.shares, keyID)
	}
}

// heldSharesKeys returns the IDs of the unseal keys this server holds.
func (m *sealManager) heldSharesKeys() []string {
	m.keysLock.Lock()
	defer m.keysLock.Unlock()
	ids := make([]string, 0, len(m.shares))
	for id := range m.shares {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// clusterConfig returns the seal configuration in the state, or nil.
func (m *sealManager) clusterConfig() *structs.SealConfig {
	if m.srv.fsm == nil {
		return nil
	}
	cfg, err := m.srv.fsm.State().SealConfig(nil)
	if err != nil {
		m.logger.Error("failed to read the seal configuration", "error", err)
		return nil
	}
	return cfg
}

// current returns the seals of the region and the rewrap in progress. A
// region without a seal configuration uses the aead seal, unless it is a new
// region of servers configured with a shares seal (no seals: uninitialized).
func (m *sealManager) current() ([]*structs.SealInfo, *structs.SealRewrap) {
	if cfg := m.clusterConfig(); cfg != nil {
		return cfg.Seals, cfg.Rewrap
	}
	// Before the seal configuration is replicated (a restarted server
	// replays its Raft log after it starts), the keystore tells: a key
	// wrapped only by a shares seal is never handled as aead.
	if m.srv.encrypter != nil {
		if keyID := m.srv.encrypter.keystoreSharesKeyID(); keyID != "" {
			return []*structs.SealInfo{{
				Type:      structs.SealTypeShares,
				KeyID:     keyID,
				Threshold: m.cfg.Threshold,
				Shares:    m.cfg.Shares,
			}}, nil
		}
	}
	if m.cfg.Type == structs.SealTypeShares && !m.keyringInitialized() {
		return nil, nil
	}
	return []*structs.SealInfo{{Type: structs.SealTypeAEAD}}, nil
}

// realSeal reports whether the region has a seal other than aead, in place,
// as the target of a rewrap, or as the configured seal of a new region: key
// material then travels between servers only wrapped by such a seal.
func (m *sealManager) realSeal() bool {
	seals, rewrap := m.current()
	if len(seals) == 0 && rewrap == nil {
		return true
	}
	return (&structs.SealConfig{Seals: seals, Rewrap: rewrap}).RealSeal()
}

// keyringInitialized reports whether the region or this server has a root
// key.
func (m *sealManager) keyringInitialized() bool {
	if m.srv.encrypter != nil && m.srv.encrypter.hasKeys() {
		return true
	}
	if m.srv.fsm == nil {
		return false
	}
	meta, err := m.srv.fsm.State().GetActiveRootKeyMeta(nil)
	return err == nil && meta != nil
}

// sealFor returns the seal of this server for a seal of the region, or nil
// for an unknown seal type or a shares seal whose key it does not hold.
func (m *sealManager) sealFor(info *structs.SealInfo) seal.Seal {
	switch info.Type {
	case structs.SealTypeAEAD:
		return m.aead
	case structs.SealTypeShares:
		if s := m.sharesKey(info.KeyID); s != nil {
			return s
		}
	}
	return nil
}

// available reports whether a seal of the region can wrap and unwrap on
// this server.
func (m *sealManager) available(info *structs.SealInfo) bool {
	s := m.sealFor(info)
	return s != nil && s.Available()
}

// sealed reports whether this server can't use the keys of the keyring.
func (m *sealManager) sealed() bool {
	seals, rewrap := m.current()
	if len(seals) > 0 {
		return !slices.ContainsFunc(seals, m.available)
	}
	// A new region: unsealed once the seal its first rewrap generates is
	// available.
	return rewrap == nil || !slices.ContainsFunc(rewrap.To, m.available)
}

// initialized reports whether the region has a seal.
func (m *sealManager) initialized() bool {
	seals, rewrap := m.current()
	return len(seals) > 0 || rewrap != nil
}

// wrapSeals returns the seals new keys are wrapped with: every seal of the
// region, which must all be available, and the available seals of a rewrap
// in progress (the rewrap adds the missing ones to every key).
func (m *sealManager) wrapSeals() ([]seal.Seal, error) {
	seals, rewrap := m.current()
	var out []seal.Seal
	add := func(info *structs.SealInfo) {
		s := m.sealFor(info)
		if s != nil && !slices.ContainsFunc(out, func(o seal.Seal) bool {
			return o.Type() == s.Type() && o.KeyID() == s.KeyID()
		}) {
			out = append(out, s)
		}
	}
	for _, info := range seals {
		if !m.available(info) {
			return nil, fmt.Errorf("%w: the %s seal is not unsealed", structs.ErrSealed, info.Type)
		}
		add(info)
	}
	if rewrap != nil {
		for _, info := range rewrap.To {
			if m.available(info) {
				add(info)
			}
		}
	}
	if len(out) == 0 {
		if !m.initialized() {
			return nil, fmt.Errorf("%w: the shares seal is not initialized yet; "+
				"generate the unseal shares with 'norca operator keyring rewrap -to shares'", structs.ErrSealed)
		}
		return nil, fmt.Errorf("%w: no seal is available", structs.ErrSealed)
	}
	return out, nil
}

// transferSeals returns the seals a key is wrapped with to send it to
// another server of a region with a real seal: the seals of wrapSeals other
// than aead, whose wraps would carry the key in usable form.
func (m *sealManager) transferSeals() ([]seal.Seal, error) {
	seals, err := m.wrapSeals()
	if err != nil {
		return nil, err
	}
	seals = slices.DeleteFunc(seals, func(s seal.Seal) bool { return s.Type() == structs.SealTypeAEAD })
	if len(seals) == 0 {
		return nil, fmt.Errorf("%w: this server holds no seal other than aead to wrap the key for another server",
			structs.ErrSealed)
	}
	return seals, nil
}

// lookup returns the seal that unwraps a wrap.
func (m *sealManager) lookup(w *structs.SealWrappedKey) seal.Seal {
	switch w.Seal {
	case structs.SealTypeAEAD:
		return m.aead
	case structs.SealTypeShares:
		if s := m.sharesKey(w.KeyID); s != nil {
			return s
		}
		return m.sealedShares
	}
	return nil
}

// WrapKey wraps a key with the seals of new keys (seal.Keyring).
func (m *sealManager) WrapKey(ctx context.Context, plaintext, aad []byte) ([]*structs.SealWrappedKey, error) {
	seals, err := m.wrapSeals()
	if err != nil {
		return nil, err
	}
	return seal.WrapAll(ctx, seals, plaintext, aad)
}

// UnwrapKey unwraps a key with any available seal (seal.Keyring).
func (m *sealManager) UnwrapKey(ctx context.Context, wraps []*structs.SealWrappedKey, aad []byte) ([]byte, error) {
	return seal.UnwrapAny(ctx, m.lookup, wraps, aad)
}

// canUnwrap reports whether this server holds the seal of a wrap.
func (m *sealManager) canUnwrap(w *structs.SealWrappedKey) bool {
	s := m.lookup(w)
	return s != nil && s.Available()
}

// sharesInfo returns the description of the unseal key a sealed server is
// unsealed with: that of the region's shares seal, or in a new region that
// of its first rewrap. Before the configuration is replicated it comes from
// the wraps in the keystore with the Shamir parameters of the agent
// configuration.
func (m *sealManager) sharesInfo() *structs.SealInfo {
	seals, rewrap := m.current()
	all := slices.Clone(seals)
	if rewrap != nil {
		all = append(all, rewrap.To...)
	}
	for _, info := range all {
		if info.Type == structs.SealTypeShares {
			return info
		}
	}
	return nil
}

// unseal enters one unseal share. Once the threshold is reached and the key
// is verified, the keystore is unwrapped and the secret store core unseals.
func (m *sealManager) unseal(share []byte) error {
	info := m.sharesInfo()
	if info == nil {
		return errors.New("the region has no shares seal to unseal")
	}
	if m.available(info) {
		return nil
	}
	threshold := info.Threshold
	if threshold == 0 {
		threshold = m.cfg.Threshold
	}
	m.unsealer.SetInfo(info)
	_, unsealed, err := m.unsealer.Unseal(share, threshold, func(key []byte) error {
		if len(info.KeyCheck) > 0 {
			if seal.CheckKey(info, key) {
				return nil
			}
			return seal.ErrUnsealFailed
		}
		if m.srv.encrypter != nil && m.srv.encrypter.verifySharesKey(info.KeyID, key) {
			return nil
		}
		return seal.ErrUnsealFailed
	})
	if err != nil {
		return err
	}
	if !unsealed {
		return nil
	}
	_, key := m.unsealer.Key()
	m.unsealer.Clear()
	err = m.addSharesKey(info, key)
	clear(key)
	if err != nil {
		return err
	}
	m.logger.Info("server unsealed", "key_id", info.KeyID)
	m.onKeyAdded()
	return nil
}

// onKeyAdded unwraps the keys of the keystore that were sealed and wakes
// everything waiting for the seal.
func (m *sealManager) onKeyAdded() {
	if m.srv.encrypter != nil {
		if err := m.srv.encrypter.unsealKeystore(); err != nil {
			// Keys wrapped only by a seal the region left (a server that was
			// down during a rewrap) are fetched, wrapped, from the other
			// servers by the keyring replication.
			m.logger.Warn("some keys of the keystore are not wrapped by a seal this server holds; "+
				"they are replicated from the other servers", "error", err)
		}
	}
	m.notify()
}

// status returns the seal status of this server.
func (m *sealManager) status() *structs.ServerSealStatus {
	s := m.srv
	seals, rewrap := m.current()
	st := &structs.ServerSealStatus{
		ID:             string(s.config.RaftConfig.LocalID),
		Name:           s.config.NodeName,
		Leader:         s.IsLeader(),
		Sealed:         m.sealed(),
		Initialized:    m.initialized(),
		Seals:          structs.SealInfoTypes(seals),
		ConfiguredSeal: m.cfg.Type,
		KeyIDs:         m.heldSharesKeys(),
	}
	if len(st.KeyIDs) == 0 {
		st.KeyIDs = nil
	}
	if s.config.ServerRPCAdvertise != nil {
		st.Address = s.config.ServerRPCAdvertise.String()
	}
	if info := m.sharesInfo(); info != nil {
		st.Threshold, st.Shares = info.Threshold, info.Shares
		if st.Threshold == 0 {
			st.Threshold, st.Shares = m.cfg.Threshold, m.cfg.Shares
		}
		st.Progress = m.unsealer.Progress()
	}
	if c := s.secretStoreCore; c != nil {
		ctx, cancel := context.WithTimeout(s.shutdownCtx, 2*time.Second)
		defer cancel()
		st.SecretCoreInitialized, _ = c.Initialized(ctx)
		st.SecretCoreSealed = c.Barrier().Sealed()
		st.SecretCoreActive = c.Active()
	}
	if env := s.secretCoreEnvelope(); env != nil {
		st.SecretCoreSeals = env.Seals()
		st.SecretCoreRecovery = coreNeedsRecovery(env, seals, rewrap)
		if st.SecretCoreRecovery && st.Leader {
			st.RecoveryProgress = m.recoveryProgress()
		}
	}
	return st
}

// rewrapKeyAAD binds the wraps of the key of a new seal in a rewrap record
// to that key.
func rewrapKeyAAD(sealType, keyID string) []byte {
	return []byte("norca-seal-rewrap-key:" + sealType + ":" + keyID)
}

// startRewrap starts a rewrap of the region's keys to another seal on the
// leader. A rewrap to shares generates the unseal key and its shares, which
// are returned once; a rewrap from shares to shares replaces the unseal key
// (a rekey). In a new region with a shares seal the first rewrap
// initializes the seal.
//
// The key of a new shares seal reaches the other servers only in the rewrap
// record in Raft (structs.SealRewrap.Keys), wrapped by the active root key of
// the keyring: every server unsealed with a current seal holds the root key
// and unwraps it there, while on disk the root keys are only wrapped by the
// seals. (A wrap by the aead seal would leave the key usable in the Raft log
// of every server; the root key is protected by the new seal once the rewrap
// has rewritten the keystores.) A new region has no root key yet, so its
// other servers are unsealed with the new shares.
func (m *sealManager) startRewrap(args *structs.KeyringRewrapRequest) (*structs.KeyringRewrapResponse, error) {
	m.rewrapLock.Lock()
	defer m.rewrapLock.Unlock()

	s := m.srv
	if !ServersMeetMinimumNorcaVersion(s.Members(), s.Region(), false) {
		return nil, fmt.Errorf("all servers must be running Norca %v or later to rewrap the keyring", minNorcaFeaturesVersion)
	}
	seals, rewrap := m.current()
	if rewrap != nil {
		return nil, fmt.Errorf("rewrap %s to %v is in progress: wait for every server to finish (norca operator seal-status)",
			rewrap.ID, structs.SealInfoTypes(rewrap.To))
	}
	if len(seals) == 1 && seals[0].Type == args.To && args.To != structs.SealTypeShares {
		return nil, fmt.Errorf("the keys are already wrapped by the %s seal", args.To)
	}
	if len(seals) > 0 && m.sealed() {
		return nil, fmt.Errorf("%w: unseal the leader before the rewrap", structs.ErrSealed)
	}

	resp := &structs.KeyringRewrapResponse{}
	var target *structs.SealInfo
	var keys []*structs.SealKeyWraps
	switch args.To {
	case structs.SealTypeAEAD:
		target = &structs.SealInfo{Type: structs.SealTypeAEAD}
	case structs.SealTypeShares:
		shares, threshold := args.Shares, args.Threshold
		if m.cfg.Type == structs.SealTypeShares {
			if shares == 0 {
				shares = m.cfg.Shares
			}
			if threshold == 0 {
				threshold = m.cfg.Threshold
			}
		}
		if shares == 0 {
			shares = config.DefaultKeyringSealShares
		}
		if threshold == 0 {
			threshold = min(config.DefaultKeyringSealThreshold, shares)
		}
		info, key, parts, err := seal.Generate(shares, threshold)
		if err != nil {
			return nil, err
		}
		defer clear(key)
		if active, _ := s.State().GetActiveRootKeyMeta(nil); len(seals) > 0 && active != nil && s.encrypter != nil {
			// the key of the new seal reaches the other servers wrapped by
			// the active root key, which the current seals protect (without
			// a root key yet, the servers are unsealed with the new shares)
			w, err := s.encrypter.wrapWithActiveKey(key, rewrapKeyAAD(info.Type, info.KeyID))
			if err != nil {
				return nil, fmt.Errorf("failed to wrap the key of the new seal: %w", err)
			}
			keys = []*structs.SealKeyWraps{{Seal: info.Type, KeyID: info.KeyID, Wraps: []*structs.SealWrappedKey{w}}}
		}
		if err := m.addSharesKey(info, key); err != nil {
			return nil, err
		}
		for _, p := range parts {
			resp.Shares = append(resp.Shares, base64.StdEncoding.EncodeToString(p))
			clear(p)
		}
		target = info
	case "":
		return nil, errors.New("the seal to rewrap to is required")
	default:
		if err := (&config.KeyringConfig{Seals: []*config.KeyringSealConfig{{Type: args.To}}}).Validate(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("unsupported seal %q", args.To)
	}

	cfg := &structs.SealConfig{
		Seals: seals,
		Rewrap: &structs.SealRewrap{
			ID:        uuid.Generate(),
			To:        []*structs.SealInfo{target},
			StartTime: time.Now().UnixNano(),
			Keys:      keys,
		},
	}
	if old := m.clusterConfig(); old != nil {
		cfg.LastRewrapID = old.LastRewrapID
	}
	_, index, err := s.raftApply(structs.SealConfigUpsertRequestType|structs.IgnoreUnknownTypeFlag,
		&structs.SealConfigUpsertRequest{Config: cfg})
	if err != nil {
		if target.Type == structs.SealTypeShares {
			m.dropSharesKey(target.KeyID)
		}
		return nil, err
	}
	m.notify()
	m.logger.Info("keyring rewrap started", "rewrap_id", cfg.Rewrap.ID,
		"from", structs.SealInfoTypes(seals), "to", args.To)

	resp.RewrapID = cfg.Rewrap.ID
	resp.Config = cfg.Stub()
	resp.Index = index
	return resp, nil
}

// runSeal reconciles the keystore and the secret store core of this server
// with the seal configuration of the region until shutdown: it takes part
// in rewraps, drops the wraps of seals that are no longer used, and on the
// leader completes rewraps.
func (m *sealManager) run(ctx context.Context) {
	for {
		store := m.srv.fsm.State()
		ws := memdb.NewWatchSet()
		ws.Add(store.AbandonCh())
		ws.Add(m.changedCh())
		cfg, err := store.SealConfig(ws)
		if err == nil && cfg != nil {
			m.lock.Lock()
			seen := cfg.ModifyIndex == m.configIndex
			m.configIndex = cfg.ModifyIndex
			m.lock.Unlock()
			if !seen {
				// wake what waits for the seal (initializeKeyring, the
				// secret store core) on every new configuration
				m.notify()
				ws = memdb.NewWatchSet()
				ws.Add(store.AbandonCh())
				ws.Add(m.changedCh())
				cfg, err = store.SealConfig(ws)
			}
		}
		if err == nil && cfg != nil {
			err = m.reconcile(ctx, cfg)
		}
		wait := 30 * time.Second
		if cfg != nil && cfg.Rewrap != nil {
			// the leader notices servers that went down during a rewrap
			wait = 3 * time.Second
		}
		if err != nil {
			if err.Error() != m.lastErr {
				m.logger.Warn("failed to reconcile the keyring seal; retrying", "error", err)
			}
			m.lastErr = err.Error()
			wait = 2 * time.Second
		} else {
			m.lastErr = ""
		}
		wctx, cancel := context.WithTimeout(ctx, wait)
		_ = ws.WatchCtx(wctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
	}
}

// reconcile brings this server in line with a seal configuration.
func (m *sealManager) reconcile(ctx context.Context, cfg *structs.SealConfig) error {
	s := m.srv
	if rw := cfg.Rewrap; rw != nil {
		if !slices.Contains(rw.Done, m.localID()) {
			if err := m.rewrapLocal(ctx, rw); err != nil {
				return err
			}
		}
		if s.IsLeader() {
			return m.completeRewrap(cfg)
		}
		return nil
	}

	// No rewrap in progress: drop the wraps of seals that are no longer
	// used, then the unseal keys of shares seals the region left. Outside a
	// rewrap no aead wrap is ever added: a configuration that does not match
	// the keystore (the restored one, for the moment between a snapshot
	// restore and the re-applied configuration of the region) can't weaken
	// it, while a server that was down during a rewrap away from aead wraps
	// its keys with the new seal once it is unsealed.
	if m.sealed() {
		return nil
	}
	if s.encrypter != nil {
		if err := s.encrypter.rewrapKeystore(false); err != nil {
			return err
		}
	}
	if s.IsLeader() {
		if err := s.rewrapSecretCoreKey(ctx, false); err != nil {
			return err
		}
	}
	m.dropUnusedSharesKeys(cfg)
	return nil
}

// dropUnusedSharesKeys forgets the unseal keys of shares seals that are not
// seals of the region and wrap no key of the keystore any more: after a
// rewrap completes, the old shares stop working.
func (m *sealManager) dropUnusedSharesKeys(cfg *structs.SealConfig) {
	for _, id := range m.heldSharesKeys() {
		if slices.ContainsFunc(cfg.Seals, func(i *structs.SealInfo) bool {
			return i.Type == structs.SealTypeShares && i.KeyID == id
		}) {
			continue
		}
		if m.srv.encrypter != nil && m.srv.encrypter.keystoreUsesSeal(structs.SealTypeShares, id) {
			continue
		}
		m.dropSharesKey(id)
		m.logger.Info("dropped the unseal key of a shares seal the region no longer uses", "key_id", id)
	}
}

// localID is the Raft ID of this server, as rewraps record it.
func (m *sealManager) localID() string {
	return string(m.srv.config.RaftConfig.LocalID)
}

// errRewrapSealed is returned when this server can't take part in a rewrap:
// it holds no seal that unwraps the key of the new seal.
var errRewrapSealed = errors.New("this server holds no seal that unwraps the key of the new seal")

// rewrapLocal wraps the keys of this server with the seals of a rewrap and
// reports it to the leader. A server that can't take part (sealed, or a
// server of a new region) reports that instead; it is unsealed with the new
// shares later.
func (m *sealManager) rewrapLocal(ctx context.Context, rw *structs.SealRewrap) error {
	s := m.srv
	seals, _ := m.current()
	if len(seals) > 0 && m.sealed() {
		return m.reportSealed(rw)
	}
	for _, info := range rw.To {
		if info.Type != structs.SealTypeShares || m.available(info) {
			continue
		}
		key, err := m.rewrapKeyFromRecord(ctx, rw, info)
		if errors.Is(err, errRewrapSealed) {
			return m.reportSealed(rw)
		}
		if err != nil {
			return fmt.Errorf("failed to unwrap the key of the %s seal of rewrap %s: %w", info.Type, rw.ID, err)
		}
		err = m.addSharesKey(info, key)
		clear(key)
		if err != nil {
			return err
		}
		m.onKeyAdded()
	}
	if s.encrypter != nil {
		if err := s.encrypter.rewrapKeystore(true); err != nil {
			return err
		}
	}
	if s.IsLeader() {
		if err := s.rewrapSecretCoreKey(ctx, true); err != nil {
			return err
		}
	}
	return m.reportRewrap(rw, false)
}

// rewrapKeyFromRecord unwraps the key of a seal of a rewrap from the rewrap
// record with a seal this server holds.
func (m *sealManager) rewrapKeyFromRecord(ctx context.Context, rw *structs.SealRewrap, info *structs.SealInfo) ([]byte, error) {
	i := slices.IndexFunc(rw.Keys, func(k *structs.SealKeyWraps) bool {
		return k.Seal == info.Type && k.KeyID == info.KeyID
	})
	if i < 0 || m.srv.encrypter == nil {
		return nil, errRewrapSealed
	}
	aad := rewrapKeyAAD(info.Type, info.KeyID)
	var key []byte
	err := errRewrapSealed
	for _, w := range rw.Keys[i].Wraps {
		if w == nil || w.Seal != structs.SealWrapKeyring {
			continue
		}
		// An unsealed server that lacks the root key (just rotated) retries
		// once the keyring replication has it.
		key, err = m.srv.encrypter.unwrapWithRootKey(w, aad)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	if len(info.KeyCheck) > 0 && !seal.CheckKey(info, key) {
		clear(key)
		return nil, errors.New("the key in the rewrap record does not match the key check of the new seal")
	}
	return key, nil
}

// reportSealed tells the leader once per rewrap that this server can't
// take part in it.
func (m *sealManager) reportSealed(rw *structs.SealRewrap) error {
	if slices.Contains(rw.Sealed, m.localID()) {
		return nil
	}
	m.lock.Lock()
	reported := m.reportedSealed == rw.ID
	m.lock.Unlock()
	if reported {
		return nil
	}
	if err := m.reportRewrap(rw, true); err != nil {
		return err
	}
	m.lock.Lock()
	m.reportedSealed = rw.ID
	m.lock.Unlock()
	return nil
}

// reportRewrap reports to the leader that this server finished a rewrap,
// or that it is sealed and can't take part.
func (m *sealManager) reportRewrap(rw *structs.SealRewrap, sealed bool) error {
	s := m.srv
	req := &structs.SealRewrapDoneRequest{
		RewrapID:     rw.ID,
		ServerID:     m.localID(),
		Sealed:       sealed,
		WriteRequest: structs.WriteRequest{Region: s.Region()},
	}
	var resp structs.GenericResponse
	if err := s.RPC("Operator.SealRewrapDone", req, &resp); err != nil {
		return fmt.Errorf("failed to report the rewrap to the leader: %w", err)
	}
	if sealed {
		m.logger.Warn("this server is sealed and does not take part in the keyring rewrap; "+
			"unseal it with the shares of the new seal once the rewrap is complete", "rewrap_id", rw.ID)
	} else {
		m.logger.Info("keys wrapped with the new seal", "rewrap_id", rw.ID, "to", structs.SealInfoTypes(rw.To))
	}
	return nil
}

// aliveServers returns the Raft IDs and addresses of the servers of the
// region that are alive in Serf.
func (m *sealManager) aliveServers() map[string]bool {
	s := m.srv
	alive := map[string]bool{}
	if s.serf == nil {
		return alive
	}
	for _, member := range s.serf.Members() {
		ok, parts := isNomadServer(member)
		if !ok || parts.Region != s.Region() || member.Status != serf.StatusAlive {
			continue
		}
		alive[parts.ID] = true
		alive[parts.Addr.String()] = true
	}
	return alive
}

// completeRewrap makes the seals of a rewrap the seals of the region (and
// drops the key wraps of the rewrap record) once the leader has wrapped the
// keys and the key of the secret store core with them, and every other
// server of the Raft configuration has finished, is sealed and can't take
// part, or is down. A server that did not take part is unsealed with the
// shares of the new seal when it returns and fetches the keys, wrapped, from
// the other servers; the old shares stop working.
func (m *sealManager) completeRewrap(cfg *structs.SealConfig) error {
	s := m.srv
	rw := cfg.Rewrap
	if !slices.Contains(rw.Done, m.localID()) {
		return nil
	}
	future := s.raft.GetConfiguration()
	if err := future.Error(); err != nil {
		return err
	}
	alive := m.aliveServers()
	var skipped []string
	for _, server := range future.Configuration().Servers {
		id := string(server.ID)
		switch {
		case slices.Contains(rw.Done, id):
		case slices.Contains(rw.Sealed, id):
			skipped = append(skipped, id+" (sealed)")
		case !alive[id] && !alive[string(server.Address)]:
			skipped = append(skipped, id+" (down)")
		default:
			return nil
		}
	}
	next := &structs.SealConfig{
		Seals:        rw.To,
		LastRewrapID: rw.ID,
	}
	if _, _, err := s.raftApply(structs.SealConfigUpsertRequestType|structs.IgnoreUnknownTypeFlag,
		&structs.SealConfigUpsertRequest{Config: next}); err != nil {
		return err
	}
	m.logger.Info("keyring rewrap complete", "rewrap_id", rw.ID, "seals", structs.SealInfoTypes(next.Seals))
	if len(skipped) > 0 {
		m.logger.Warn("servers that did not take part in the rewrap must be unsealed with the shares of the new seal",
			"rewrap_id", rw.ID, "servers", strings.Join(skipped, ", "))
	}
	return nil
}

// waitUnsealed blocks until the seal of this server can wrap keys, or stopCh
// is closed. It returns false if stopped.
func (m *sealManager) waitUnsealed(stopCh <-chan struct{}) bool {
	logged := false
	for {
		changed := m.changedCh()
		if _, err := m.wrapSeals(); err == nil {
			return true
		} else if !logged {
			m.logger.Warn("waiting for the keyring seal", "error", err)
			logged = true
		}
		select {
		case <-stopCh:
			return false
		case <-changed:
		case <-time.After(5 * time.Second):
		}
	}
}

// keptSealConfig returns the seal configuration a restore of a Raft
// snapshot must keep: the keystores of the servers are not part of the
// snapshot, so the seals that wrap them must not be rolled back. A region
// with the implicit aead seal keeps it explicitly.
func (m *sealManager) keptSealConfig() *structs.SealConfig {
	if cfg := m.clusterConfig(); cfg != nil {
		return cfg.Copy()
	}
	seals, rewrap := m.current()
	if rewrap == nil && len(seals) == 1 && seals[0].Type == structs.SealTypeAEAD {
		return &structs.SealConfig{Seals: []*structs.SealInfo{{Type: structs.SealTypeAEAD}}}
	}
	return nil
}

// reapplySealConfig writes a seal configuration of before a restore of a
// Raft snapshot (keptSealConfig) over the one of the restored state.
func (s *Server) reapplySealConfig(before *structs.SealConfig) error {
	if before == nil {
		return nil
	}
	restored, err := s.State().SealConfig(nil)
	if err != nil {
		return err
	}
	switch {
	case restored == nil && before.ModifyIndex == 0:
		// the implicit aead seal, before and after
		return nil
	case restored != nil && restored.ModifyIndex == before.ModifyIndex:
		return nil
	}
	_, _, err = s.raftApply(structs.SealConfigUpsertRequestType|structs.IgnoreUnknownTypeFlag,
		&structs.SealConfigUpsertRequest{Config: before})
	return err
}

// coreNeedsRecovery reports whether the stored key of a secret store core
// is wrapped by none of the seals of the region (a restore of a backup
// taken with other seals). The aead seal unwraps anywhere.
func coreNeedsRecovery(env *seal.CoreEnvelope, seals []*structs.SealInfo, rewrap *structs.SealRewrap) bool {
	if env == nil {
		return false
	}
	all := slices.Clone(seals)
	if rewrap != nil {
		all = append(all, rewrap.To...)
	}
	for _, w := range env.Wraps {
		if w == nil {
			continue
		}
		if w.Seal == structs.SealTypeAEAD {
			return false
		}
		if slices.ContainsFunc(all, func(i *structs.SealInfo) bool { return i.Type == w.Seal && i.KeyID == w.KeyID }) {
			return false
		}
	}
	return true
}

// secretStoreSealWarning returns a warning for a full restore if the secret
// store of a backup is wrapped by seals the region does not have, and how
// the restore recovers it: with the secret store key object of the backup,
// or, for backups without one (taken before Norca wrote it, or without the
// keyring), with the old shares.
func secretStoreSealWarning(env *seal.CoreEnvelope, current []*structs.SealInfo, hasCoreKey bool) string {
	if !coreNeedsRecovery(env, current, nil) {
		return ""
	}
	msg := fmt.Sprintf("the secret store of the backup is wrapped by seals %v that this region does not use", env.Seals())
	if hasCoreKey {
		return msg + ": the restore re-wraps its key, from the secret store key of the backup, with the seals of the region"
	}
	return msg + " and the backup has no copy of its key (taken before Norca recorded it, or without the keyring): " +
		"its core stays sealed after the restore until an operator enters the old unseal shares " +
		"with 'norca operator unseal -recover'"
}

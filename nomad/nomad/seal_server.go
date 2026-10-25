// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/seal"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
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
// The aead seal is always available; a shares seal is available once the
// unseal key is reconstructed from shares (Operator.SealUnseal), or handed
// over by another server during a rewrap.
type sealManager struct {
	srv    *Server
	logger hclog.Logger

	// cfg is the seal of the agent configuration, with defaults.
	cfg *config.KeyringSealConfig

	aead   seal.AEAD
	shares *seal.Shares

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
}

func newSealManager(s *Server, cfg *config.KeyringConfig) *sealManager {
	m := &sealManager{
		srv:     s,
		logger:  s.logger.Named("seal"),
		cfg:     cfg.Seal(),
		shares:  seal.NewShares(),
		changed: make(chan struct{}),
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
// for an unknown seal type.
func (m *sealManager) sealFor(info *structs.SealInfo) seal.Seal {
	switch info.Type {
	case structs.SealTypeAEAD:
		return m.aead
	case structs.SealTypeShares:
		return m.shares
	}
	return nil
}

// available reports whether a seal of the region can wrap and unwrap on
// this server.
func (m *sealManager) available(info *structs.SealInfo) bool {
	s := m.sealFor(info)
	if s == nil || !s.Available() {
		return false
	}
	return info.Type != structs.SealTypeShares || s.KeyID() == info.KeyID
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
		if s != nil && !slices.ContainsFunc(out, func(o seal.Seal) bool { return o.Type() == s.Type() }) {
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

// lookup returns the seal that unwraps a wrap.
func (m *sealManager) lookup(w *structs.SealWrappedKey) seal.Seal {
	switch w.Seal {
	case structs.SealTypeAEAD:
		return m.aead
	case structs.SealTypeShares:
		return m.shares
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

// wantedWrap reports whether a wrap belongs to a seal keys are wrapped with
// now; the others are dropped once no rewrap is in progress.
func (m *sealManager) wantedWrap(w *structs.SealWrappedKey) bool {
	seals, rewrap := m.current()
	match := func(info *structs.SealInfo) bool {
		return info.Type == w.Seal && (info.Type != structs.SealTypeShares || info.KeyID == w.KeyID)
	}
	if slices.ContainsFunc(seals, match) {
		return true
	}
	return rewrap != nil && slices.ContainsFunc(rewrap.To, match)
}

// sharesInfo returns the description of the unseal key of the region's
// shares seal: from the seal configuration, or, before the configuration is
// replicated, from the wraps in the keystore with the Shamir parameters of
// the agent configuration.
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
	m.shares.SetInfo(info)
	_, unsealed, err := m.shares.Unseal(share, threshold, func(key []byte) error {
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
	if unsealed {
		m.logger.Info("server unsealed", "key_id", info.KeyID)
		m.onKeyAdded()
	}
	return nil
}

// onKeyAdded unwraps the keys of the keystore that were sealed and wakes
// everything waiting for the seal.
func (m *sealManager) onKeyAdded() {
	if m.srv.encrypter != nil {
		if err := m.srv.encrypter.unsealKeystore(); err != nil {
			m.logger.Error("failed to unwrap the keystore", "error", err)
		}
	}
	m.notify()
}

// status returns the seal status of this server.
func (m *sealManager) status() *structs.ServerSealStatus {
	s := m.srv
	seals, _ := m.current()
	st := &structs.ServerSealStatus{
		ID:             string(s.config.RaftConfig.LocalID),
		Name:           s.config.NodeName,
		Leader:         s.IsLeader(),
		Sealed:         m.sealed(),
		Initialized:    m.initialized(),
		Seals:          structs.SealInfoTypes(seals),
		ConfiguredSeal: m.cfg.Type,
	}
	if s.config.ServerRPCAdvertise != nil {
		st.Address = s.config.ServerRPCAdvertise.String()
	}
	if info := m.sharesInfo(); info != nil {
		st.Threshold, st.Shares = info.Threshold, info.Shares
		if st.Threshold == 0 {
			st.Threshold, st.Shares = m.cfg.Threshold, m.cfg.Shares
		}
		st.Progress = m.shares.Progress()
	}
	if m.shares.Available() {
		st.KeyIDs = []string{m.shares.KeyID()}
	}
	if c := s.secretStoreCore; c != nil {
		ctx, cancel := context.WithTimeout(s.shutdownCtx, 2*time.Second)
		defer cancel()
		st.SecretCoreInitialized, _ = c.Initialized(ctx)
		st.SecretCoreSealed = c.Barrier().Sealed()
		st.SecretCoreActive = c.Active()
	}
	return st
}

// startRewrap starts a rewrap of the region's keys to another seal on the
// leader. A rewrap to shares generates the unseal key and its shares, which
// are returned once. In a new region with a shares seal the first rewrap
// initializes the seal.
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
	if len(seals) == 1 && seals[0].Type == args.To {
		return nil, fmt.Errorf("the keys are already wrapped by the %s seal", args.To)
	}
	if len(seals) > 0 && m.sealed() {
		return nil, fmt.Errorf("%w: unseal the leader before the rewrap", structs.ErrSealed)
	}

	resp := &structs.KeyringRewrapResponse{}
	var target *structs.SealInfo
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
		if err := m.shares.SetKey(info, key); err != nil {
			return nil, err
		}
		for i := range key {
			key[i] = 0
		}
		for _, p := range parts {
			resp.Shares = append(resp.Shares, base64.StdEncoding.EncodeToString(p))
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
		},
	}
	if old := m.clusterConfig(); old != nil {
		cfg.LastRewrapID = old.LastRewrapID
	}
	_, index, err := s.raftApply(structs.SealConfigUpsertRequestType|structs.IgnoreUnknownTypeFlag,
		&structs.SealConfigUpsertRequest{Config: cfg})
	if err != nil {
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
	for _, info := range append(slices.Clone(cfg.Seals), rewrapTargets(cfg)...) {
		if info.Type == structs.SealTypeShares {
			m.shares.SetInfo(info)
		}
	}

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
	// used, and the unseal key of a shares seal the region left.
	if !slices.ContainsFunc(cfg.Seals, func(i *structs.SealInfo) bool { return i.Type == structs.SealTypeShares }) &&
		m.shares.Available() {
		m.shares.Clear()
		m.logger.Info("dropped the unseal key of the shares seal: the region no longer uses it")
	}
	if m.sealed() {
		return nil
	}
	if s.encrypter != nil {
		if err := s.encrypter.rewrapKeystore(); err != nil {
			return err
		}
	}
	if s.IsLeader() {
		return s.rewrapSecretCoreKey(ctx)
	}
	return nil
}

func rewrapTargets(cfg *structs.SealConfig) []*structs.SealInfo {
	if cfg.Rewrap == nil {
		return nil
	}
	return cfg.Rewrap.To
}

// localID is the Raft ID of this server, as rewraps record it.
func (m *sealManager) localID() string {
	return string(m.srv.config.RaftConfig.LocalID)
}

// rewrapLocal wraps the keys of this server with the seals of a rewrap and
// reports it to the leader.
func (m *sealManager) rewrapLocal(ctx context.Context, rw *structs.SealRewrap) error {
	s := m.srv
	seals, _ := m.current()
	if len(seals) > 0 && m.sealed() {
		// a sealed server takes part once an operator unsealed it
		return nil
	}
	for _, info := range rw.To {
		if info.Type != structs.SealTypeShares || m.available(info) {
			continue
		}
		key, err := m.fetchRewrapKey(rw.ID, info.KeyID)
		if err != nil {
			return fmt.Errorf("failed to get the key of the %s seal of rewrap %s: %w", info.Type, rw.ID, err)
		}
		err = m.shares.SetKey(info, key)
		for i := range key {
			key[i] = 0
		}
		if err != nil {
			return err
		}
		m.onKeyAdded()
	}
	if s.encrypter != nil {
		if err := s.encrypter.rewrapKeystore(); err != nil {
			return err
		}
	}
	if s.IsLeader() {
		if err := s.rewrapSecretCoreKey(ctx); err != nil {
			return err
		}
	}

	req := &structs.SealRewrapDoneRequest{
		RewrapID:     rw.ID,
		ServerID:     m.localID(),
		WriteRequest: structs.WriteRequest{Region: s.Region()},
	}
	var resp structs.GenericResponse
	if err := s.RPC("Operator.SealRewrapDone", req, &resp); err != nil {
		return fmt.Errorf("failed to report the rewrap to the leader: %w", err)
	}
	m.logger.Info("keys wrapped with the new seal", "rewrap_id", rw.ID, "to", structs.SealInfoTypes(rw.To))
	return nil
}

// fetchRewrapKey gets the key of a seal of a rewrap from the leader, or from
// any other server that has it.
func (m *sealManager) fetchRewrapKey(rewrapID, keyID string) ([]byte, error) {
	s := m.srv
	req := &structs.SealRewrapKeyRequest{
		RewrapID:     rewrapID,
		KeyID:        keyID,
		QueryOptions: structs.QueryOptions{Region: s.Region()},
	}
	var resp structs.SealRewrapKeyResponse
	err := s.RPC("Operator.SealRewrapKey", req, &resp)
	if err == nil && len(resp.Key) > 0 {
		return resp.Key, nil
	}
	req.AllowStale = true
	for _, peer := range s.keyringReplicator.getAllPeers() {
		if peer.ID == s.config.NodeID {
			continue
		}
		resp = structs.SealRewrapKeyResponse{}
		if err = s.forwardServer(peer, "Operator.SealRewrapKey", req, &resp); err == nil && len(resp.Key) > 0 {
			return resp.Key, nil
		}
	}
	if err == nil {
		err = errors.New("no server has the key")
	}
	return nil, err
}

// rewrapKey returns the key of a seal of the rewrap in progress, for
// another server taking part in it.
func (m *sealManager) rewrapKey(rewrapID, keyID string) ([]byte, error) {
	_, rw := m.current()
	if rw == nil || rw.ID != rewrapID {
		return nil, fmt.Errorf("rewrap %s is not in progress", rewrapID)
	}
	if !slices.ContainsFunc(rw.To, func(i *structs.SealInfo) bool {
		return i.Type == structs.SealTypeShares && i.KeyID == keyID
	}) {
		return nil, fmt.Errorf("rewrap %s has no seal key %s", rewrapID, keyID)
	}
	info, key := m.shares.Key()
	if info == nil || info.KeyID != keyID {
		return nil, nil
	}
	return key, nil
}

// completeRewrap makes the seals of a rewrap the seals of the region once
// every server of the Raft configuration has finished it.
func (m *sealManager) completeRewrap(cfg *structs.SealConfig) error {
	s := m.srv
	future := s.raft.GetConfiguration()
	if err := future.Error(); err != nil {
		return err
	}
	for _, server := range future.Configuration().Servers {
		if !slices.Contains(cfg.Rewrap.Done, string(server.ID)) {
			return nil
		}
	}
	next := &structs.SealConfig{
		Seals:        cfg.Rewrap.To,
		LastRewrapID: cfg.Rewrap.ID,
	}
	if _, _, err := s.raftApply(structs.SealConfigUpsertRequestType|structs.IgnoreUnknownTypeFlag,
		&structs.SealConfigUpsertRequest{Config: next}); err != nil {
		return err
	}
	m.logger.Info("keyring rewrap complete", "rewrap_id", cfg.Rewrap.ID, "seals", structs.SealInfoTypes(next.Seals))
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

// reapplySealConfig writes a seal configuration of before a full restore
// over the one of the restored state: the keystores of the servers are not
// restored, so the seals that wrap them must not be rolled back.
func (s *Server) reapplySealConfig(before *structs.SealConfig) error {
	if before == nil {
		return nil
	}
	restored, err := s.State().SealConfig(nil)
	if err != nil {
		return err
	}
	if restored != nil && restored.ModifyIndex == before.ModifyIndex {
		return nil
	}
	_, _, err = s.raftApply(structs.SealConfigUpsertRequestType|structs.IgnoreUnknownTypeFlag,
		&structs.SealConfigUpsertRequest{Config: before})
	return err
}

// secretStoreSealWarning returns a warning for a full restore if the secret
// store of a backup is wrapped by a seal key the region does not have: its
// core then stays sealed after the restore. The aead seal unwraps anywhere.
func secretStoreSealWarning(backupSeals, current []*structs.SealInfo) string {
	if len(backupSeals) == 0 {
		return ""
	}
	for _, b := range backupSeals {
		if b.Type == structs.SealTypeAEAD {
			return ""
		}
		for _, c := range current {
			if c.Type == b.Type && c.KeyID == b.KeyID {
				return ""
			}
		}
	}
	return fmt.Sprintf("the secret store of the backup is wrapped by seals %v that this region does not use: "+
		"its core stays sealed after the restore", structs.SealInfoTypes(backupSeals))
}

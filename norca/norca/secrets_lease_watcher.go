// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	metrics "github.com/hashicorp/go-metrics"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"

	"github.com/bacanoo-io/norca/norca/norca/secretengine"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The lease watcher runs on the leader and ends the leases of the dynamic
// secrets (norca-doc content/docs/design/secrets-plan.md, "Rotation and
// dynamic secrets"). The leases are in Raft, so a new leader's watcher takes
// over where the former leader's stopped. On each change of the leases or
// engines tables, of an allocation holding a lease, and at the next known
// expiry (at least once a minute), it:
//
//   - revokes the leases whose allocation stopped (is client terminal or
//     gone), that expired without renewal, or that reached their max_ttl;
//   - removes revoked leases once their credential has expired (the end of
//     max_ttl), when the backend no longer needs to know them;
//   - sends each engine's operator the revoked credentials whenever they
//     changed, and again every secretLeaseResyncInterval. Failed calls are
//     retried with a backoff.
//
// Every revocation is an audit record (lease-revoke, accessor
// "secret-engine") and a SecretLeaseRevoked event of the Secret topic.

const (
	// secretLeaseWatcherInterval is the longest time between two
	// evaluations.
	secretLeaseWatcherInterval = time.Minute

	// secretLeaseResyncInterval is how often the backends are brought in
	// line without a change.
	secretLeaseResyncInterval = 5 * time.Minute

	// The backoff of failed backend pushes doubles from the minimum up to
	// the maximum.
	secretSyncBackoffMin = 5 * time.Second
	secretSyncBackoffMax = 5 * time.Minute
)

// secretEngineSyncState is the state of the pushes to an engine's backend.
type secretEngineSyncState struct {
	// fingerprint describes the state last pushed successfully, at.
	fingerprint string
	at          time.Time

	failures int
	next     time.Time
}

// secretLeaseWatcher is the state of the lease watcher of one leadership
// term. It is used by one goroutine only.
type secretLeaseWatcher struct {
	srv    *Server
	logger hclog.Logger

	interval time.Duration
	resync   time.Duration

	// now is replaced in tests.
	now func() time.Time

	synced map[secretEngineKey]*secretEngineSyncState
}

func newSecretLeaseWatcher(s *Server) *secretLeaseWatcher {
	return &secretLeaseWatcher{
		srv:      s,
		logger:   s.logger.Named("secret_lease_watcher"),
		interval: secretLeaseWatcherInterval,
		resync:   secretLeaseResyncInterval,
		now:      time.Now,
		synced:   map[secretEngineKey]*secretEngineSyncState{},
	}
}

// runSecretLeaseWatcher runs the lease watcher until stopCh is closed, which
// happens when the server loses leadership.
func (s *Server) runSecretLeaseWatcher(stopCh chan struct{}) {
	if s.config.secretLeaseWatcherDisabled {
		return
	}
	newSecretLeaseWatcher(s).run(stopCh)
}

func (w *secretLeaseWatcher) run(stopCh <-chan struct{}) {
	defer metrics.SetGauge([]string{"nomad", "secrets", "leases", "active"}, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	for {
		ws := memdb.NewWatchSet()
		next := w.evaluate(ctx, ws)

		wait := w.interval
		if !next.IsZero() {
			if d := next.Sub(w.now()); d < wait {
				wait = max(d, 100*time.Millisecond)
			}
		}
		waitCtx, waitCancel := context.WithTimeout(ctx, wait)
		_ = ws.WatchCtx(waitCtx)
		waitCancel()
		if ctx.Err() != nil {
			return
		}
	}
}

// evaluate checks every lease and engine once and returns the next time
// something is due (zero if nothing is known). ws watches the tables and
// the allocations of the live leases.
func (w *secretLeaseWatcher) evaluate(ctx context.Context, ws memdb.WatchSet) time.Time {
	store := w.srv.State()
	now := w.now()
	var next time.Time
	soon := func(t time.Time) {
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	retry := now.Add(w.interval)

	iter, err := store.SecretLeases(ws)
	if err != nil {
		w.logger.Error("failed to list the secret leases", "error", err)
		return retry
	}
	var revoke []*structs.SecretLease
	var purge []string
	active := 0
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		l := raw.(*structs.SecretLease)
		if l.Revoked {
			if now.UnixNano() >= l.MaxExpireTime {
				purge = append(purge, l.ID)
			} else {
				soon(time.Unix(0, l.MaxExpireTime))
			}
			continue
		}
		alloc, err := store.AllocByID(ws, l.AllocID)
		if err != nil {
			w.logger.Error("failed to look up the allocation of a lease", "lease_id", l.ID, "error", err)
			soon(retry)
			continue
		}
		switch {
		case alloc == nil || alloc.ClientTerminalStatus():
			revoke = append(revoke, secretLeaseRevoked(l, structs.SecretLeaseRevokeStopped, now))
		case now.UnixNano() >= l.ExpireTime && l.ExpireTime >= l.MaxExpireTime:
			revoke = append(revoke, secretLeaseRevoked(l, structs.SecretLeaseRevokeMaxTTL, now))
		case now.UnixNano() >= l.ExpireTime:
			revoke = append(revoke, secretLeaseRevoked(l, structs.SecretLeaseRevokeExpired, now))
		default:
			active++
			soon(time.Unix(0, l.ExpireTime))
		}
	}
	metrics.SetGauge([]string{"nomad", "secrets", "leases", "active"}, float32(active))

	if len(revoke) > 0 {
		if err := w.revoke(revoke); err != nil {
			soon(now.Add(secretSyncBackoffMin))
		}
	}
	if len(purge) > 0 {
		if _, _, err := w.srv.raftApply(structs.SecretLeasePurgeRequestType|structs.IgnoreUnknownTypeFlag,
			&structs.SecretLeasePurgeRequest{IDs: purge}); err != nil {
			w.logger.Warn("failed to remove ended secret leases", "error", err)
			soon(now.Add(secretSyncBackoffMin))
		}
	}

	engines, err := store.SecretEngines(ws)
	if err != nil {
		w.logger.Error("failed to list the secret engines", "error", err)
		return retry
	}
	seen := map[secretEngineKey]struct{}{}
	for raw := engines.Next(); raw != nil; raw = engines.Next() {
		engine := raw.(*structs.SecretEngine)
		key := secretEngineKey{engine.Namespace, engine.Name}
		seen[key] = struct{}{}
		if t := w.engine(ctx, engine, now); !t.IsZero() {
			soon(t)
		}
	}
	for key := range w.synced {
		if _, ok := seen[key]; !ok {
			delete(w.synced, key)
		}
	}
	return next
}

// revoke revokes leases through Raft, with an audit record for each.
func (w *secretLeaseWatcher) revoke(leases []*structs.SecretLease) error {
	_, err := w.srv.secretLeaseApply(leases)
	for _, l := range leases {
		rec := &structs.SecretAccessEvent{
			Region:    w.srv.Region(),
			Server:    w.srv.config.NodeName,
			Namespace: l.Namespace,
			Path:      l.CredsPath(),
			Operation: structs.SecretOpLeaseRevoke,
			Accessor: &structs.SecretAccessor{
				System:       secretEngineAccessor,
				Namespace:    l.Namespace,
				JobID:        l.JobID,
				TaskGroup:    l.TaskGroup,
				Task:         l.Task,
				AllocationID: l.AllocID,
				NodeID:       l.NodeID,
			},
			Engine:  l.Engine,
			Role:    l.Role,
			LeaseID: l.ID,
			Reason:  l.RevokeReason,
		}
		rec.Result = secretAccessResult(err)
		if err != nil {
			rec.Error = err.Error()
		}
		_ = w.srv.secretAudit(rec, true)
		if err == nil {
			metrics.IncrCounterWithLabels([]string{"nomad", "secrets", "leases", "revoked"}, 1,
				[]metrics.Label{{Name: "engine", Value: l.Engine}, {Name: "reason", Value: l.RevokeReason}})
		}
	}
	if err != nil {
		w.logger.Warn("failed to revoke secret leases", "leases", len(leases), "error", err)
		return err
	}
	w.logger.Debug("revoked secret leases", "leases", len(leases))
	return nil
}

// engine brings the backend of an engine in line. It returns when to look again (zero
// if nothing is due before the next resync).
func (w *secretLeaseWatcher) engine(ctx context.Context, engine *structs.SecretEngine, now time.Time) time.Time {
	key := secretEngineKey{engine.Namespace, engine.Name}
	st := w.synced[key]
	if st == nil {
		st = &secretEngineSyncState{}
		w.synced[key] = st
	}
	logger := w.logger.With("namespace", engine.Namespace, "engine", engine.Name)

	if _, err := w.srv.secretCore(); err != nil {
		logger.Debug("postponing the secret engine sync, the secret store is unavailable", "error", err)
		return now.Add(w.interval)
	}
	if now.Before(st.next) {
		return st.next
	}

	lock := w.srv.secrets.engineLock(engine.Namespace, engine.Name)
	lock.Lock()
	defer lock.Unlock()

	// The engine may have changed while the lock was taken.
	current, err := w.srv.State().SecretEngineByName(nil, engine.Namespace, engine.Name)
	if err != nil || current == nil {
		return time.Time{}
	}
	engine = current.Copy()

	revoked, err := w.srv.secretEngineRevocations(engine.Namespace, engine.Name, now)
	if err != nil {
		return now.Add(secretSyncBackoffMin)
	}
	fingerprint := secretEngineFingerprint(w.srv, engine, revoked)
	if fingerprint == st.fingerprint && now.Sub(st.at) < w.resync {
		return st.at.Add(w.resync)
	}

	err = w.sync(ctx, engine, revoked)
	if err != nil {
		st.failures++
		backoff := secretSyncBackoffMax
		if st.failures <= 7 {
			backoff = min(secretSyncBackoffMin<<(st.failures-1), secretSyncBackoffMax)
		}
		st.next = now.Add(backoff)
		metrics.IncrCounterWithLabels([]string{"nomad", "secrets", "engine", "sync_failed"}, 1,
			[]metrics.Label{{Name: "engine", Value: engine.Name}})
		logger.Warn("failed to bring the secret engine's backend in line with its leases",
			"attempt", st.failures, "retry_in", backoff, "error", err)
		return st.next
	}
	if st.failures > 0 {
		logger.Info("brought the secret engine's backend in line with its leases again")
	}
	st.fingerprint, st.at, st.failures, st.next = fingerprint, now, 0, time.Time{}
	metrics.IncrCounterWithLabels([]string{"nomad", "secrets", "engine", "synced"}, 1,
		[]metrics.Label{{Name: "engine", Value: engine.Name}})
	return now.Add(w.resync)
}

// sync brings the backend of an engine in line. The caller holds the
// engine's lock.
func (w *secretLeaseWatcher) sync(ctx context.Context, engine *structs.SecretEngine, revoked map[string]time.Time) error {
	backend, err := secretengine.For(engine.Type)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, secretEngineTimeout)
	defer cancel()
	return backend.Sync(ctx, &secretengine.SyncRequest{Engine: engine, Revoked: revoked,
		Call: w.srv.secretOperatorCall(engine)})
}

// secretEngineFingerprint describes the state a sync of an engine brings
// the backend to: the engine's version and the revocations.
func secretEngineFingerprint(s *Server, engine *structs.SecretEngine, revoked map[string]time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d", engine.ModifyIndex)
	keys := make([]string, 0, len(revoked))
	for k, at := range revoked {
		keys = append(keys, fmt.Sprintf("%s@%d", k, at.Unix()))
	}
	sort.Strings(keys)
	b.WriteString("|" + strings.Join(keys, ","))
	return b.String()
}

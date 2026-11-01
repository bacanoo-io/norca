// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/bacanoo-io/norca/nomad/helper/uuid"
	"github.com/bacanoo-io/norca/nomad/nomad/secretengine"
	"github.com/bacanoo-io/norca/nomad/nomad/structs"
)

// The dynamic secrets of the built-in engine on the leader (norca-doc
// content/docs/design/secrets-plan.md, "Rotation and dynamic secrets"):
// creating credentials, rotating the admin credential of an engine and
// bringing the backends in line with the leases. Everything runs on the
// leader, the only server whose secret store core can read the engines'
// leases. The writes of an engine are serialized by the engine's lock.

const (
	// secretEngineAccessor is the system accessor of the audit records of
	// the engines' reads of their admin credentials and of the leader's own
	// lease operations.
	secretEngineAccessor = "secret-engine"

	// secretEngineTimeout bounds a backend operation.
	secretEngineTimeout = 30 * time.Second
)

// secretEngineKey identifies an engine.
type secretEngineKey struct {
	namespace, name string
}

// engineLock returns the lock of an engine, created on first use.
func (ss *serverSecrets) engineLock(namespace, name string) *sync.Mutex {
	ss.engineLocksLock.Lock()
	defer ss.engineLocksLock.Unlock()
	if ss.engineLocks == nil {
		ss.engineLocks = map[secretEngineKey]*sync.Mutex{}
	}
	key := secretEngineKey{namespace, name}
	l := ss.engineLocks[key]
	if l == nil {
		l = &sync.Mutex{}
		ss.engineLocks[key] = l
	}
	return l
}

// secretEngineRevocations returns the backend references of the revoked
// leases of an engine whose credentials have not expired yet.
func (s *Server) secretEngineRevocations(namespace, engine string, now time.Time) (map[string]time.Time, error) {
	iter, err := s.State().SecretLeasesByEngine(nil, namespace, engine)
	if err != nil {
		return nil, err
	}
	out := map[string]time.Time{}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		l := raw.(*structs.SecretLease)
		if l.Revoked && l.BackendRef != "" && now.UnixNano() < l.MaxExpireTime {
			out[l.BackendRef] = time.Unix(0, l.RevokeTime)
		}
	}
	return out, nil
}

// secretEngineSyncRequest builds the state an engine's backend is brought in
// line with.
func (s *Server) secretEngineSyncRequest(engine *structs.SecretEngine) (*secretengine.SyncRequest, error) {
	revoked, err := s.secretEngineRevocations(engine.Namespace, engine.Name, time.Now())
	if err != nil {
		return nil, err
	}
	return &secretengine.SyncRequest{Engine: engine, Revoked: revoked, Call: s.secretOperatorCall(engine)}, nil
}

// secretBackendError is the coded error of a failed backend operation.
func secretBackendError(engine *structs.SecretEngine, op string, err error) error {
	return structs.NewErrRPCCodedf(http.StatusBadGateway, "secret engine %q: %s failed: %v", engine.Name, op, err)
}

// secretEngineUpsert validates an engine with its admin credential, brings
// the backend in line with it and writes it. existing engines keep their
// state and creation time.
func (s *Server) secretEngineUpsert(engine *structs.SecretEngine, checkIndex *uint64) (*structs.SecretEngine, uint64, error) {
	lock := s.secrets.engineLock(engine.Namespace, engine.Name)
	lock.Lock()
	defer lock.Unlock()

	existing, err := s.State().SecretEngineByName(nil, engine.Namespace, engine.Name)
	if err != nil {
		return nil, 0, err
	}
	var current uint64
	if existing != nil {
		current = existing.ModifyIndex
		if existing.Type != engine.Type {
			return nil, 0, structs.NewErrRPCCodedf(http.StatusBadRequest,
				"the type of engine %q can't be changed (%s); delete and recreate it", engine.Name, existing.Type)
		}
		engine.CreateTime = existing.CreateTime
	}
	if checkIndex != nil && *checkIndex != current {
		return nil, 0, structs.ErrSecretConflict
	}
	now := time.Now()
	engine.ModifyTime = now.UnixNano()
	if existing == nil {
		engine.CreateTime = engine.ModifyTime
	}

	backend, err := secretengine.For(engine.Type)
	if err != nil {
		return nil, 0, structs.NewErrRPCCoded(http.StatusBadRequest, err.Error())
	}
	syncReq, err := s.secretEngineSyncRequest(engine)
	if err != nil {
		return nil, 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), secretEngineTimeout)
	defer cancel()
	if err := backend.Sync(ctx, syncReq); err != nil {
		return nil, 0, secretBackendError(engine, "configuring the backend", err)
	}
	return s.secretEngineApply(engine, current)
}

// secretEngineApply writes an engine through Raft.
func (s *Server) secretEngineApply(engine *structs.SecretEngine, expected uint64) (*structs.SecretEngine, uint64, error) {
	resp, index, err := s.raftApply(structs.SecretEngineApplyRequestType|structs.IgnoreUnknownTypeFlag,
		&structs.SecretEngineApplyRequest{Engine: engine, ExpectedIndex: expected})
	if err != nil {
		return nil, 0, err
	}
	if err, ok := resp.(error); ok && err != nil {
		return nil, 0, err
	}
	out, err := s.State().SecretEngineByName(nil, engine.Namespace, engine.Name)
	if err != nil {
		return nil, 0, err
	}
	return out.Copy(), index, nil
}

// secretEngineDelete removes an engine without live leases, with its ended
// leases.
func (s *Server) secretEngineDelete(namespace, name string, checkIndex *uint64) (uint64, error) {
	lock := s.secrets.engineLock(namespace, name)
	lock.Lock()
	defer lock.Unlock()

	existing, err := s.State().SecretEngineByName(nil, namespace, name)
	if err != nil {
		return 0, err
	}
	if existing == nil {
		return 0, structs.ErrSecretEngineNotFound
	}
	if checkIndex != nil && *checkIndex != existing.ModifyIndex {
		return 0, structs.ErrSecretConflict
	}
	iter, err := s.State().SecretLeasesByEngine(nil, namespace, name)
	if err != nil {
		return 0, err
	}
	live := 0
	now := time.Now()
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		if raw.(*structs.SecretLease).Live(now) {
			live++
		}
	}
	if live > 0 {
		return 0, structs.NewErrRPCCodedf(http.StatusConflict,
			"secret engine %q has %d live leases; revoke them first (norca secret lease revoke -engine %s)", name, live, name)
	}
	resp, index, err := s.raftApply(structs.SecretEnginePurgeRequestType|structs.IgnoreUnknownTypeFlag,
		&structs.SecretEnginePurgeRequest{Namespace: namespace, Name: name, ExpectedIndex: existing.ModifyIndex})
	if err != nil {
		return 0, err
	}
	if err, ok := resp.(error); ok && err != nil {
		return 0, err
	}
	return index, nil
}

// secretLeaseCreate creates a credential of a role for the task of alloc and
// records its lease. The credential is returned to the caller only.
func (s *Server) secretLeaseCreate(alloc *structs.Allocation, task string, args *structs.SecretLeaseCreateRequest) (*structs.SecretLease, *secretengine.Credential, uint64, error) {
	engine, err := s.State().SecretEngineByName(nil, alloc.Namespace, args.Engine)
	if err != nil {
		return nil, nil, 0, err
	}
	if engine == nil {
		return nil, nil, 0, structs.ErrSecretEngineNotFound
	}
	role := engine.Role(args.Role)
	if role == nil {
		return nil, nil, 0, structs.NewErrRPCCodedf(http.StatusNotFound,
			"secret engine %q has no role %q", args.Engine, args.Role)
	}
	return s.secretLeaseCreateOperator(engine, role, alloc, task, args)
}

// secretLeaseApply writes leases through Raft.
func (s *Server) secretLeaseApply(leases []*structs.SecretLease) (uint64, error) {
	resp, index, err := s.raftApply(structs.SecretLeaseApplyRequestType|structs.IgnoreUnknownTypeFlag,
		&structs.SecretLeaseApplyRequest{Leases: leases})
	if err != nil {
		return 0, err
	}
	if err, ok := resp.(error); ok && err != nil {
		return 0, err
	}
	return index, nil
}

// newSecretLease returns a lease of a role for a task.
func newSecretLease(engine *structs.SecretEngine, role *structs.SecretEngineRole, alloc *structs.Allocation, task string,
	args *structs.SecretLeaseCreateRequest, now time.Time) *structs.SecretLease {

	return &structs.SecretLease{
		ID:            uuid.Generate(),
		Namespace:     alloc.Namespace,
		Engine:        engine.Name,
		EngineType:    engine.Type,
		Role:          role.Name,
		SecretName:    args.SecretName,
		AllocID:       alloc.ID,
		JobID:         alloc.JobID,
		TaskGroup:     alloc.TaskGroup,
		Task:          task,
		NodeID:        alloc.NodeID,
		TTL:           role.TTL,
		MaxTTL:        role.MaxTTL,
		IssueTime:     now.UnixNano(),
		ExpireTime:    now.Add(role.TTL).UnixNano(),
		MaxExpireTime: now.Add(role.MaxTTL).UnixNano(),
	}
}

// secretLeaseRenew extends a live lease by its TTL, up to its max_ttl.
func (s *Server) secretLeaseRenew(lease *structs.SecretLease) (*structs.SecretLease, structs.SecretItems, uint64, error) {
	now := time.Now()
	if !lease.Live(now) {
		return nil, nil, 0, structs.ErrSecretLeaseRevoked
	}
	result, err := s.secretLeaseRenewBackend(lease)
	if err != nil {
		return nil, nil, 0, err
	}
	renewed := lease.Copy()
	var items structs.SecretItems
	if result != nil {
		items = result.Items
		if !result.Expires.IsZero() {
			// A replaced credential has its own end; the old limit does
			// not hold for it.
			if len(items) > 0 {
				renewed.MaxExpireTime = result.Expires.UnixNano()
			} else {
				renewed.MaxExpireTime = min(renewed.MaxExpireTime, result.Expires.UnixNano())
			}
		}
	}
	renewed.ExpireTime = min(now.Add(lease.TTL).UnixNano(), renewed.MaxExpireTime)
	renewed.RenewTime = now.UnixNano()
	renewed.Renewals++
	index, err := s.secretLeaseApply([]*structs.SecretLease{renewed})
	if err != nil {
		return nil, nil, 0, err
	}
	out, err := s.State().SecretLeaseByID(nil, lease.ID)
	if err != nil || out == nil {
		return nil, nil, 0, errors.Join(structs.ErrSecretLeaseNotFound, err)
	}
	return out.Copy(), items, index, nil
}

// secretLeaseRevoked returns the revoked copy of a lease.
func secretLeaseRevoked(lease *structs.SecretLease, reason string, now time.Time) *structs.SecretLease {
	out := lease.Copy()
	out.Revoked = true
	out.RevokeTime = now.UnixNano()
	out.RevokeReason = reason
	return out
}

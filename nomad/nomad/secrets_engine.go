// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/secretengine"
	"github.com/hashicorp/nomad/nomad/structs"
)

// The dynamic secrets of the built-in engine on the leader (norca-doc
// content/docs/design/secrets-plan.md, "Rotation and dynamic secrets"):
// creating credentials, rotating the admin credential of an engine and
// bringing the backends in line with the leases. Everything runs on the
// leader, the only server whose secret store core can read the engines'
// admin credentials. The operations of one engine are serialized by the
// engine's lock: a credential is never signed by a key that a concurrent
// rotate-root or push is about to change.

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

// secretEngineCredential reads the admin credential of an engine from the
// leader's core. The read is an audit record of the engine (accessor
// "secret-engine"); the operation it serves is recorded separately.
func (s *Server) secretEngineCredential(engine *structs.SecretEngine) (structs.SecretItems, error) {
	rec := &structs.SecretAccessEvent{
		Region:    s.Region(),
		Server:    s.config.NodeName,
		Namespace: engine.Namespace,
		Path:      engine.CredentialPath,
		Operation: structs.SecretOpRead,
		Accessor:  &structs.SecretAccessor{System: secretEngineAccessor},
		Engine:    engine.Name,
	}
	if err := s.secretAudit(rec, false); err != nil {
		return nil, err
	}
	items, version, err := s.secretReadValue(engine.Namespace, engine.CredentialPath, 0)
	rec.Version = version
	rec.Result = secretAccessResult(err)
	if err != nil {
		rec.Error = err.Error()
	}
	if auditErr := s.secretAudit(rec, true); auditErr != nil && err == nil {
		return nil, auditErr
	}
	if err != nil {
		if code, _, ok := structs.CodeFromRPCCodedErr(secretCodedError(err)); ok && code == http.StatusNotFound {
			return nil, structs.NewErrRPCCodedf(http.StatusBadRequest,
				"the admin credential %q of engine %q is not readable: %v", engine.CredentialPath, engine.Name, err)
		}
		return nil, secretCodedError(err)
	}
	return items, nil
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
func (s *Server) secretEngineSyncRequest(engine *structs.SecretEngine, cred structs.SecretItems) (*secretengine.SyncRequest, error) {
	revoked, err := s.secretEngineRevocations(engine.Namespace, engine.Name, time.Now())
	if err != nil {
		return nil, err
	}
	return &secretengine.SyncRequest{Engine: engine, Credential: cred, Revoked: revoked}, nil
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
		engine.State = existing.State.Copy()
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
	cred, err := s.secretEngineCredential(engine)
	if err != nil {
		return nil, 0, err
	}
	if err := backend.Validate(engine, cred); err != nil {
		return nil, 0, structs.NewErrRPCCodedf(http.StatusBadRequest, "secret engine %q: %v", engine.Name, err)
	}
	syncReq, err := s.secretEngineSyncRequest(engine, cred)
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

// secretEngineRotateRoot rotates the admin key of an engine: the backend
// creates the new key and accepts it next to the current one, the new admin
// credential is written as a new version of the engine's credential secret,
// and the old key is retired. The leader removes a retired key from the
// backend once the leases it signed have ended. The write of the new
// credential version is an audit record of the engine (accessor
// "secret-engine"); the rotate-root itself is recorded by the caller.
func (s *Server) secretEngineRotateRoot(namespace, name string) (*structs.SecretEngine, string, uint64, error) {
	lock := s.secrets.engineLock(namespace, name)
	lock.Lock()
	defer lock.Unlock()

	engine, err := s.State().SecretEngineByName(nil, namespace, name)
	if err != nil {
		return nil, "", 0, err
	}
	if engine == nil {
		return nil, "", 0, structs.ErrSecretEngineNotFound
	}
	engine = engine.Copy()
	backend, err := secretengine.For(engine.Type)
	if err != nil {
		return nil, "", 0, err
	}
	cred, err := s.secretEngineCredential(engine)
	if err != nil {
		return nil, "", 0, err
	}
	syncReq, err := s.secretEngineSyncRequest(engine, cred)
	if err != nil {
		return nil, "", 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), secretEngineTimeout)
	defer cancel()
	res, err := backend.RotateRoot(ctx, syncReq)
	if err != nil {
		return nil, "", 0, secretBackendError(engine, "rotate-root", err)
	}

	// The new credential becomes a new version of the engine's credential
	// secret, through the secret store's write path.
	rec := &structs.SecretAccessEvent{
		Region:    s.Region(),
		Server:    s.config.NodeName,
		Namespace: namespace,
		Path:      engine.CredentialPath,
		Operation: structs.SecretOpWrite,
		Accessor:  &structs.SecretAccessor{System: secretEngineAccessor},
		Engine:    engine.Name,
	}
	if err := s.secretAudit(rec, false); err != nil {
		return nil, "", 0, err
	}
	written, _, err := s.secretWrite(&secretWrite{namespace: namespace, path: engine.CredentialPath, items: res.Credential})
	if written != nil {
		rec.Version = written.CurrentVersion
	}
	rec.Result = secretAccessResult(err)
	if err != nil {
		rec.Error = err.Error()
	}
	_ = s.secretAudit(rec, true)
	if err != nil {
		return nil, "", 0, err
	}

	if engine.State == nil {
		engine.State = &structs.SecretEngineState{}
	}
	if res.OldKey != "" && res.OldKey != res.NewKey && !slices.Contains(engine.State.RetiredKeys, res.OldKey) {
		engine.State.RetiredKeys = append(engine.State.RetiredKeys, res.OldKey)
	}
	engine.State.RemovedKeys = slices.DeleteFunc(engine.State.RemovedKeys, func(k string) bool { return k == res.NewKey })
	engine.State.RootRotateTime = time.Now().UnixNano()
	engine.ModifyTime = engine.State.RootRotateTime
	out, index, err := s.secretEngineApply(engine, engine.ModifyIndex)
	if err != nil {
		return nil, "", 0, err
	}
	return out, res.NewKey, index, nil
}

// secretLeaseCreate creates a credential of a role for the task of alloc and
// records its lease. The credential is returned to the caller only.
func (s *Server) secretLeaseCreate(alloc *structs.Allocation, task string, args *structs.SecretLeaseCreateRequest) (*structs.SecretLease, *secretengine.Credential, uint64, error) {
	lock := s.secrets.engineLock(alloc.Namespace, args.Engine)
	lock.Lock()
	defer lock.Unlock()

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
	backend, err := secretengine.For(engine.Type)
	if err != nil {
		return nil, nil, 0, err
	}
	cred, err := s.secretEngineCredential(engine)
	if err != nil {
		return nil, nil, 0, err
	}

	now := time.Now()
	lease := &structs.SecretLease{
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
	ctx, cancel := context.WithTimeout(context.Background(), secretEngineTimeout)
	defer cancel()
	c, err := backend.Issue(ctx, &secretengine.IssueRequest{
		Engine:        engine,
		Role:          role,
		Credential:    cred,
		Name:          fmt.Sprintf("norca %s/%s/%s/%s lease %s", alloc.Namespace, alloc.JobID, alloc.TaskGroup, task, lease.ID),
		IssueTime:     now,
		MaxExpireTime: time.Unix(0, lease.MaxExpireTime),
	})
	if err != nil {
		return nil, nil, 0, secretBackendError(engine, "creating the credential", err)
	}
	lease.BackendRef, lease.SigningKey = c.BackendRef, c.SigningKey

	index, err := s.secretLeaseApply([]*structs.SecretLease{lease})
	if err != nil {
		// The credential was never handed out; its seed is dropped here.
		return nil, nil, 0, err
	}
	stored, err := s.State().SecretLeaseByID(nil, lease.ID)
	if err != nil || stored == nil {
		return nil, nil, 0, fmt.Errorf("lease %s not found after its creation: %v", lease.ID, err)
	}
	return stored.Copy(), c, index, nil
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

// secretLeaseRenew extends a live lease by its TTL, up to its max_ttl.
func (s *Server) secretLeaseRenew(lease *structs.SecretLease) (*structs.SecretLease, uint64, error) {
	now := time.Now()
	if !lease.Live(now) {
		return nil, 0, structs.ErrSecretLeaseRevoked
	}
	renewed := lease.Copy()
	renewed.ExpireTime = min(now.Add(lease.TTL).UnixNano(), lease.MaxExpireTime)
	renewed.RenewTime = now.UnixNano()
	renewed.Renewals++
	index, err := s.secretLeaseApply([]*structs.SecretLease{renewed})
	if err != nil {
		return nil, 0, err
	}
	out, err := s.State().SecretLeaseByID(nil, lease.ID)
	if err != nil || out == nil {
		return nil, 0, errors.Join(structs.ErrSecretLeaseNotFound, err)
	}
	return out.Copy(), index, nil
}

// secretLeaseRevoked returns the revoked copy of a lease.
func secretLeaseRevoked(lease *structs.SecretLease, reason string, now time.Time) *structs.SecretLease {
	out := lease.Copy()
	out.Revoked = true
	out.RevokeTime = now.UnixNano()
	out.RevokeReason = reason
	return out
}

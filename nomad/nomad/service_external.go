// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"net/http"
	"sync"
	"time"

	"github.com/armon/go-metrics"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// externalServiceTick is how often the leader expires TTL checks.
	externalServiceTick = time.Second

	// externalServiceReconcileInterval is how often the leader deregisters
	// the external services that stayed critical for too long, and arms the
	// TTL checks it does not track yet.
	externalServiceReconcileInterval = 30 * time.Second
)

// externalServices maintains the external service registrations on the
// leader. Writes of external registrations (registration, check updates,
// expiry, deregistration) are serialized by it, so their read-modify-write
// cycles do not race.
//
// TTL checks are tracked in memory, like node heartbeats: a renewal resets the
// deadline of the check, and only a change of its status is written to Raft.
// A check that is not renewed within its TTL turns critical. A new leader
// gives every TTL check a full TTL from the time it took over.
type externalServices struct {
	srv    *Server
	logger hclog.Logger
	now    func() time.Time

	reconcileInterval time.Duration

	lock      sync.Mutex
	deadlines map[externalCheckKey]time.Time
	leader    bool
	reconcile time.Time
}

// externalCheckKey identifies a TTL check of an external registration.
type externalCheckKey struct {
	namespace string
	id        string
	check     string
}

func newExternalServices(srv *Server) *externalServices {
	return &externalServices{
		srv:               srv,
		logger:            srv.logger.Named("external_services"),
		now:               time.Now,
		reconcileInterval: externalServiceReconcileInterval,
		deadlines:         make(map[externalCheckKey]time.Time),
	}
}

// run expires TTL checks and garbage collects registrations while the server
// is the leader, until stopCh is closed.
func (e *externalServices) run(stopCh <-chan struct{}) {
	ticker := time.NewTicker(externalServiceTick)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			e.tick()
		}
	}
}

// tick runs one round of the leader duties.
func (e *externalServices) tick() {
	if !e.srv.IsLeader() {
		e.lock.Lock()
		if e.leader {
			e.leader = false
			e.deadlines = make(map[externalCheckKey]time.Time)
		}
		e.lock.Unlock()
		return
	}

	e.lock.Lock()
	defer e.lock.Unlock()

	now := e.now()
	if !e.leader {
		// a new leader: give every TTL check a full TTL
		e.leader = true
		e.deadlines = make(map[externalCheckKey]time.Time)
		e.reconcile = time.Time{}
	}
	if now.After(e.reconcile) {
		e.reconcile = now.Add(e.reconcileInterval)
		if err := e.reconcileLocked(now); err != nil {
			e.logger.Warn("failed to reconcile external services", "error", err)
		}
	}
	if err := e.expireLocked(now); err != nil {
		e.logger.Warn("failed to expire TTL checks", "error", err)
	}
}

// externalRegistrations returns every external registration in state.
func externalRegistrations(snap *state.StateStore) ([]*structs.ServiceRegistration, error) {
	iter, err := snap.GetServiceRegistrations(nil)
	if err != nil {
		return nil, err
	}
	var out []*structs.ServiceRegistration
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		if reg := raw.(*structs.ServiceRegistration); reg.IsExternal() {
			out = append(out, reg)
		}
	}
	return out, nil
}

// reconcileLocked arms the TTL checks that have no deadline, drops the
// deadlines of checks that are gone, and deregisters the registrations that
// stayed critical longer than their deregister_critical_after.
func (e *externalServices) reconcileLocked(now time.Time) error {
	regs, err := externalRegistrations(e.srv.fsm.State())
	if err != nil {
		return err
	}

	present := make(map[externalCheckKey]bool)
	for _, reg := range regs {
		dca := reg.External.DeregisterCriticalAfter
		if dca > 0 && reg.Health == structs.ServiceHealthCritical &&
			now.Sub(time.Unix(0, reg.HealthUpdatedAt)) >= dca {
			e.logger.Info("deregistering external service critical for too long",
				"namespace", reg.Namespace, "service_id", reg.ID, "deregister_critical_after", dca)
			if _, err := e.deleteLocked(reg.Namespace, reg.ID); err != nil {
				e.logger.Warn("failed to deregister external service", "service_id", reg.ID, "error", err)
			} else {
				metrics.IncrCounter([]string{"nomad", "service_registration", "external_reaped"}, 1)
			}
			continue
		}
		for _, check := range reg.External.Checks {
			if check.Type != structs.ExternalCheckTypeTTL {
				continue
			}
			key := externalCheckKey{reg.Namespace, reg.ID, check.ID}
			present[key] = true
			if _, ok := e.deadlines[key]; !ok {
				if state := reg.CheckState(check.ID); state != nil && state.Status != structs.ServiceHealthCritical {
					e.deadlines[key] = now.Add(check.TTL)
				}
			}
		}
	}
	for key := range e.deadlines {
		if !present[key] {
			delete(e.deadlines, key)
		}
	}
	return nil
}

// expireLocked turns the TTL checks whose deadline passed critical.
func (e *externalServices) expireLocked(now time.Time) error {
	expired := make(map[structs.NamespacedID][]string)
	for key, deadline := range e.deadlines {
		if now.Before(deadline) {
			continue
		}
		delete(e.deadlines, key)
		nid := structs.NamespacedID{Namespace: key.namespace, ID: key.id}
		expired[nid] = append(expired[nid], key.check)
	}
	if len(expired) == 0 {
		return nil
	}

	snap := e.srv.fsm.State()
	var apply []*structs.ServiceRegistration
	for nid, checks := range expired {
		current, err := snap.GetServiceRegistrationByID(nil, nid.Namespace, nid.ID)
		if err != nil {
			return err
		}
		if !current.IsExternal() {
			continue
		}
		reg := current.Copy()
		for _, id := range checks {
			if state := reg.CheckState(id); state != nil {
				state.Status = structs.ServiceHealthCritical
				state.Output = structs.ExternalCheckTTLExpiredOutput
			}
		}
		reg.SetExternalHealth(current, now)
		if reg.HealthEqual(current) {
			continue
		}
		e.logger.Debug("external service TTL check expired", "namespace", reg.Namespace,
			"service_id", reg.ID, "checks", checks)
		apply = append(apply, reg)
	}
	if len(apply) == 0 {
		return nil
	}
	metrics.IncrCounter([]string{"nomad", "service_registration", "external_ttl_expired"}, float32(len(apply)))
	_, err := e.writeLocked(apply)
	return err
}

// writeLocked writes registrations through Raft.
func (e *externalServices) writeLocked(regs []*structs.ServiceRegistration) (uint64, error) {
	args := &structs.ServiceRegistrationUpsertRequest{
		Services:     regs,
		WriteRequest: structs.WriteRequest{Region: e.srv.Region()},
	}
	_, index, err := e.srv.raftApply(structs.ServiceRegistrationUpsertRequestType, args)
	return index, err
}

// armLocked sets the deadlines of the TTL checks of reg whose status is not
// critical, and drops those of its other checks.
func (e *externalServices) armLocked(reg *structs.ServiceRegistration, now time.Time) {
	for key := range e.deadlines {
		if key.namespace == reg.Namespace && key.id == reg.ID {
			delete(e.deadlines, key)
		}
	}
	for _, check := range reg.External.Checks {
		if check.Type != structs.ExternalCheckTypeTTL {
			continue
		}
		if state := reg.CheckState(check.ID); state != nil && state.Status != structs.ServiceHealthCritical {
			e.deadlines[externalCheckKey{reg.Namespace, reg.ID, check.ID}] = now.Add(check.TTL)
		}
	}
}

// upsert writes a validated external registration. The states of checks that
// exist with the same type are kept; new checks start with their initial
// status. The registration renews its TTL checks.
func (e *externalServices) upsert(reg *structs.ServiceRegistration) (uint64, error) {
	e.lock.Lock()
	defer e.lock.Unlock()

	existing, err := e.srv.fsm.State().GetServiceRegistrationByID(nil, reg.Namespace, reg.ID)
	if err != nil {
		return 0, err
	}
	if existing != nil && !existing.IsExternal() {
		return 0, structs.NewErrRPCCodedf(http.StatusConflict,
			"service ID %q is used by a registration of allocation %s", reg.ID, existing.AllocID)
	}
	if existing != nil && existing.NodeID != reg.NodeID {
		return 0, structs.NewErrRPCCodedf(http.StatusConflict,
			"external service %q is owned by another node; deregister it first", reg.ID)
	}

	now := e.now()
	reg.Checks = make([]*structs.ServiceRegistrationCheck, 0, len(reg.External.Checks))
	for _, def := range reg.External.Checks {
		var state *structs.ServiceRegistrationCheck
		if existing != nil {
			if old, oldDef := existing.CheckState(def.ID), existing.External.Check(def.ID); old != nil &&
				oldDef != nil && oldDef.Type == def.Type {
				state = old.Copy()
				state.Name = def.Name
			}
		}
		if state == nil {
			status := def.InitialStatus
			if status == "" {
				status = structs.ServiceHealthCritical
			}
			state = &structs.ServiceRegistrationCheck{ID: def.ID, Name: def.Name, Status: status}
		}
		reg.Checks = append(reg.Checks, state)
	}
	if len(reg.Checks) == 0 {
		reg.Checks = nil
	}
	reg.SetExternalHealth(existing, now)

	index := uint64(0)
	if existing != nil && existing.Equal(reg) {
		index = existing.ModifyIndex
	} else {
		if index, err = e.writeLocked([]*structs.ServiceRegistration{reg}); err != nil {
			return 0, err
		}
	}
	if e.leader {
		e.armLocked(reg, now)
	}
	return index, nil
}

// delete deregisters an external registration.
func (e *externalServices) delete(namespace, id string) (uint64, error) {
	e.lock.Lock()
	defer e.lock.Unlock()
	return e.deleteLocked(namespace, id)
}

func (e *externalServices) deleteLocked(namespace, id string) (uint64, error) {
	existing, err := e.srv.fsm.State().GetServiceRegistrationByID(nil, namespace, id)
	if err != nil {
		return 0, err
	}
	if existing == nil {
		return 0, errExternalNotFound(namespace, id)
	}
	if !existing.IsExternal() {
		return 0, errNotExternal(id)
	}

	args := &structs.ServiceRegistrationDeleteByIDRequest{
		ID:           id,
		WriteRequest: structs.WriteRequest{Region: e.srv.Region(), Namespace: namespace},
	}
	_, index, err := e.srv.raftApply(structs.ServiceRegistrationDeleteByIDRequestType, args)
	if err != nil {
		return 0, err
	}
	for key := range e.deadlines {
		if key.namespace == namespace && key.id == id {
			delete(e.deadlines, key)
		}
	}
	return index, nil
}

// updateChecks applies check status updates. With nodeID set, they are the
// results of the http and tcp checks run by that node, which must own the
// registration; otherwise they are TTL renewals, and an update without a
// check ID applies to every TTL check. Only status changes are written; the
// output is recorded with them.
func (e *externalServices) updateChecks(namespace, id string, updates []*structs.ExternalCheckUpdate,
	nodeID string) (uint64, error) {

	e.lock.Lock()
	defer e.lock.Unlock()

	current, err := e.srv.fsm.State().GetServiceRegistrationByID(nil, namespace, id)
	if err != nil {
		return 0, err
	}
	if current == nil {
		return 0, errExternalNotFound(namespace, id)
	}
	if !current.IsExternal() {
		return 0, errNotExternal(id)
	}
	if nodeID != "" && current.NodeID != nodeID {
		return 0, structs.ErrPermissionDenied
	}
	if len(updates) == 0 {
		return 0, structs.NewErrRPCCoded(http.StatusBadRequest, "no check update")
	}

	now := e.now()
	reg := current.Copy()
	renewed := make(map[string]bool)

	for _, u := range updates {
		switch u.Status {
		case structs.ServiceHealthPassing, structs.ServiceHealthWarning, structs.ServiceHealthCritical:
		default:
			return 0, structs.NewErrRPCCodedf(http.StatusBadRequest,
				"invalid check status %q (passing, warning or critical)", u.Status)
		}

		var targets []*structs.ExternalServiceCheck
		if u.CheckID == "" {
			if nodeID != "" {
				return 0, structs.NewErrRPCCoded(http.StatusBadRequest, "missing check ID")
			}
			for _, def := range reg.External.Checks {
				if def.Type == structs.ExternalCheckTypeTTL {
					targets = append(targets, def)
				}
			}
			if len(targets) == 0 {
				return 0, structs.NewErrRPCCodedf(http.StatusBadRequest,
					"external service %q has no TTL check", id)
			}
		} else {
			def := reg.External.Check(u.CheckID)
			if def == nil {
				return 0, structs.NewErrRPCCodedf(http.StatusNotFound,
					"check %q of external service %q not found", u.CheckID, id)
			}
			if nodeID != "" && !def.IsNodeCheck() {
				return 0, structs.NewErrRPCCodedf(http.StatusBadRequest, "check %q is not run by a node", u.CheckID)
			}
			if nodeID == "" && def.Type != structs.ExternalCheckTypeTTL {
				return 0, structs.NewErrRPCCodedf(http.StatusBadRequest, "check %q is not a TTL check", u.CheckID)
			}
			targets = append(targets, def)
		}

		for _, def := range targets {
			state := reg.CheckState(def.ID)
			if state == nil {
				state = &structs.ServiceRegistrationCheck{ID: def.ID, Name: def.Name}
				reg.Checks = append(reg.Checks, state)
			}
			if state.Status != u.Status {
				state.Output = structs.TruncateCheckOutput(u.Output)
			}
			state.Status = u.Status
			if def.Type == structs.ExternalCheckTypeTTL {
				renewed[def.ID] = true
			}
		}
	}
	reg.SetExternalHealth(current, now)

	index := current.ModifyIndex
	if !reg.HealthEqual(current) {
		if index, err = e.writeLocked([]*structs.ServiceRegistration{reg}); err != nil {
			return 0, err
		}
	}

	if e.leader {
		for _, def := range reg.External.Checks {
			if !renewed[def.ID] {
				continue
			}
			key := externalCheckKey{namespace, id, def.ID}
			if state := reg.CheckState(def.ID); state != nil && state.Status != structs.ServiceHealthCritical {
				e.deadlines[key] = now.Add(def.TTL)
			} else {
				delete(e.deadlines, key)
			}
		}
	}
	return index, nil
}

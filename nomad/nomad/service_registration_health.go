// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"sync"
	"time"

	"github.com/armon/go-metrics"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// serviceHealthWindow is the minimum time between two recorded health
	// changes of one service instance. Changes coming faster are held back
	// and merged: only the last one is applied when the window has passed.
	serviceHealthWindow = 5 * time.Second

	// serviceHealthFlushInterval is how often held back changes are checked.
	serviceHealthFlushInterval = 250 * time.Millisecond
)

// serviceHealthDamper is the flapping protection of the health-based catalog.
// It runs on the leader, which receives all service registration upserts: a
// health change of an instance whose health changed less than
// serviceHealthWindow ago is not written to Raft right away but held back,
// and replaced by any later change of the same instance. When the window has
// passed, the last held back change is applied, unless the instance has gone
// back to its recorded health in the meantime.
//
// Held back changes are lost if the leader loses its leadership; clients
// re-assert the health of their instances periodically, which corrects it.
type serviceHealthDamper struct {
	srv    *Server
	logger hclog.Logger
	window time.Duration
	now    func() time.Time

	lock    sync.Mutex
	pending map[serviceHealthKey]*pendingServiceHealth
}

type serviceHealthKey struct {
	namespace string
	id        string
}

type pendingServiceHealth struct {
	reg *structs.ServiceRegistration
	due time.Time
}

func newServiceHealthDamper(srv *Server) *serviceHealthDamper {
	return &serviceHealthDamper{
		srv:     srv,
		logger:  srv.logger.Named("service_health"),
		window:  serviceHealthWindow,
		now:     time.Now,
		pending: make(map[serviceHealthKey]*pendingServiceHealth),
	}
}

// filter decides, for every registration of an upsert, whether it is written
// now. It sets HealthUpdatedAt on the registrations that are written, holds
// back the health changes that come too fast, and drops the registrations
// that would not change the state. existing looks up the current
// registration. It returns the registrations to write.
func (d *serviceHealthDamper) filter(regs []*structs.ServiceRegistration,
	existing func(namespace, id string) (*structs.ServiceRegistration, error)) ([]*structs.ServiceRegistration, error) {

	now := d.now()
	out := make([]*structs.ServiceRegistration, 0, len(regs))
	var changes, deferred int

	d.lock.Lock()
	defer d.lock.Unlock()

	for _, reg := range regs {
		key := serviceHealthKey{reg.Namespace, reg.ID}

		current, err := existing(reg.Namespace, reg.ID)
		if err != nil {
			return nil, err
		}

		switch {
		case current == nil:
			reg.HealthUpdatedAt = now.UnixNano()
			delete(d.pending, key)

		case reg.HealthEqual(current):
			// a report of the recorded health merges away the changes
			// held back since
			reg.HealthUpdatedAt = current.HealthUpdatedAt
			delete(d.pending, key)
			if reg.Equal(current) {
				continue
			}

		default:
			last := time.Unix(0, current.HealthUpdatedAt)
			if reg.EqualExceptHealth(current) && now.Sub(last) < d.window {
				d.pending[key] = &pendingServiceHealth{reg: reg.Copy(), due: last.Add(d.window)}
				deferred++
				continue
			}
			reg.HealthUpdatedAt = now.UnixNano()
			delete(d.pending, key)
			changes++
		}

		out = append(out, reg)
	}

	d.emit(changes, deferred)
	return out, nil
}

// emit records the health metrics.
func (d *serviceHealthDamper) emit(changes, deferred int) {
	labels := []metrics.Label{{Name: "region", Value: d.srv.Region()}}
	if changes > 0 {
		metrics.IncrCounterWithLabels([]string{"nomad", "service_registration", "health_updates"},
			float32(changes), labels)
	}
	if deferred > 0 {
		metrics.IncrCounterWithLabels([]string{"nomad", "service_registration", "health_updates_deferred"},
			float32(deferred), labels)
	}
}

// due removes and returns the held back changes whose window has passed.
func (d *serviceHealthDamper) due() []*structs.ServiceRegistration {
	now := d.now()

	d.lock.Lock()
	defer d.lock.Unlock()

	var out []*structs.ServiceRegistration
	for key, p := range d.pending {
		if now.Before(p.due) {
			continue
		}
		delete(d.pending, key)
		out = append(out, p.reg)
	}
	return out
}

// run applies the held back changes when their window has passed, until
// stopCh is closed.
func (d *serviceHealthDamper) run(stopCh <-chan struct{}) {
	ticker := time.NewTicker(serviceHealthFlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			d.flush()
		}
	}
}

// flush applies the held back changes whose window has passed.
func (d *serviceHealthDamper) flush() {
	regs := d.due()
	if len(regs) == 0 {
		return
	}

	if !d.srv.IsLeader() {
		d.logger.Debug("dropping held back service health changes, no longer leader", "count", len(regs))
		return
	}

	snap := d.srv.fsm.State()
	var apply []*structs.ServiceRegistration
	for _, reg := range regs {
		current, err := snap.GetServiceRegistrationByID(nil, reg.Namespace, reg.ID)
		if err != nil {
			d.logger.Error("failed to look up service registration", "service_id", reg.ID, "error", err)
			continue
		}
		// the instance was deregistered or re-registered differently since
		if current == nil || !reg.EqualExceptHealth(current) || reg.HealthEqual(current) {
			continue
		}
		reg.HealthUpdatedAt = d.now().UnixNano()
		apply = append(apply, reg)
	}
	if len(apply) == 0 {
		return
	}

	args := &structs.ServiceRegistrationUpsertRequest{
		Services:     apply,
		WriteRequest: structs.WriteRequest{Region: d.srv.Region()},
	}
	if _, _, err := d.srv.raftApply(structs.ServiceRegistrationUpsertRequestType, args); err != nil {
		d.logger.Warn("failed to apply held back service health changes", "count", len(apply), "error", err)
		return
	}
	d.emit(len(apply), 0)
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
	metrics "github.com/hashicorp/go-metrics"
)

// Built-in operators: controllers of object kinds that run in the servers,
// on the leader only, and use nothing but the core. Each watches the objects
// of its kinds and brings what they describe about; nothing has to be
// installed for them and they upgrade with the servers.
//
// Each operator has its own loop, watch set and wake time, so an operator
// that waits (for a CSI plugin, an external issuer) delays only itself. What
// the operators share is the state store, Raft and the servers' helpers
// (writeOperatorObject, operatorEvent, the metrics), which are safe to call
// from several goroutines as the RPC handlers do; an operator keeps no
// state of its own that another one reads.
//
// An operator writes the status of its objects, with a check-and-set on the
// modify index, and records what it did as object events. Work that takes
// longer than one pass (an order with an external issuer) keeps its progress
// in the status, so the next leader continues it.
//
// The conditions Ready and Degraded are common to the kinds of the
// operators: the metrics store counts the objects of every kind and those
// that are Degraded (norca.objects.count, norca.objects.degraded).

var (
	// operatorsInterval is how often the operators run when nothing they
	// watch changes. Tests lower it.
	operatorsInterval = time.Minute

	// operatorsRetry is how long the loop waits after a failure, and before
	// it tries again to register the kinds.
	operatorsRetry = 10 * time.Second

	// operatorsSettle is the pause after a change before the operators run,
	// so that a burst of writes is one pass.
	operatorsSettle = 200 * time.Millisecond
)

// coreOperator is a built-in operator.
type coreOperator interface {
	// kinds returns the kinds the operator needs registered.
	kinds() []*structs.ObjectKind

	// reconcile brings the objects of the operator to their wanted state.
	// It adds what it read to the watch set, and returns when it wants to
	// run again at the latest (the zero time: no wish).
	reconcile(ws memdb.WatchSet) (time.Time, error)
}

// operatorManager runs the built-in operators.
type operatorManager struct {
	srv    *Server
	logger hclog.Logger

	operators []coreOperator

	// mu guards run.
	mu sync.Mutex

	// run is closed when the loops of the last start have all ended. A
	// start waits for it, so the loops of two leaderships, or of a
	// leadership that is established again, never run side by side: an
	// operator makes one pass at a time, whatever happens to the
	// leadership.
	run chan struct{}
}

func newOperatorManager(srv *Server, logger hclog.Logger) *operatorManager {
	m := &operatorManager{srv: srv, logger: logger.Named("operators")}
	m.operators = []coreOperator{
		newCertificateOperator(srv, m.logger),
		newSnapshotOperator(srv, m.logger),
		newVolumeRequestOperator(srv, m.logger),
		newTenantOperator(srv, m.logger),
	}
	return m
}

// start runs the built-in operators until stopCh is closed: when this
// server becomes the leader. It does not block. Each operator has a loop of
// its own, so one that waits for something outside (the call of a CSI plugin
// may take minutes) does not hold back the others.
func (m *operatorManager) start(stopCh chan struct{}) {
	m.mu.Lock()
	previous := m.run
	done := make(chan struct{})
	m.run = done
	m.mu.Unlock()

	go func() {
		defer close(done)
		if previous != nil {
			// A pass of the last leadership may still be in flight: it
			// ends on its own, and no operator is run twice at a time.
			<-previous
		}
		m.runOperators(stopCh)
	}()
}

// stopped returns a channel that is closed when the loops of the last
// start have ended (nil: never started).
func (m *operatorManager) stopped() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.run
}

// runOperators registers the kinds of the built-in operators, runs the loop
// of each of them, and returns when all the loops have ended.
func (m *operatorManager) runOperators(stopCh chan struct{}) {
	var kinds []*structs.ObjectKind
	for _, op := range m.operators {
		kinds = append(kinds, op.kinds()...)
	}
	for {
		select {
		case <-stopCh:
			return
		default:
		}
		err := m.srv.registerKinds(kinds)
		if err == nil {
			break
		}
		m.logger.Warn("registering the kinds of the built-in operators failed, will retry", "error", err)
		select {
		case <-stopCh:
			return
		case <-time.After(operatorsRetry):
		}
	}

	var loops sync.WaitGroup
	for _, op := range m.operators {
		loops.Add(1)
		go func(op coreOperator) {
			defer loops.Done()
			m.operatorLoop(op, stopCh)
		}(op)
	}
	loops.Wait()
}

// operatorLoop runs one built-in operator while this server is the leader:
// a pass when something the last pass read changes, at the time the pass
// asked for, and every operatorsInterval at the least. The passes of an
// operator follow each other; those of different operators run side by
// side.
func (m *operatorManager) operatorLoop(op coreOperator, stopCh chan struct{}) {
	name := fmt.Sprintf("%T", op)
	if kinds := op.kinds(); len(kinds) > 0 {
		name = kinds[0].Kind
	}
	for {
		// No pass starts once the leadership is lost; one in flight ends
		// on its own.
		select {
		case <-stopCh:
			return
		default:
		}

		ws := memdb.NewWatchSet()
		now := time.Now()
		wish, err := op.reconcile(ws)
		if err != nil {
			m.logger.Warn("built-in operator failed, will retry", "operator", name, "error", err)
			wish = now.Add(operatorsRetry)
		}
		next := operatorWake(now.Add(operatorsInterval), wish, now)

		ctx, cancel := context.WithCancel(context.Background())
		timer := time.NewTimer(max(time.Until(next), operatorsSettle))
		select {
		case <-stopCh:
			timer.Stop()
			cancel()
			return
		case <-timer.C:
			cancel()
		case <-ws.WatchCh(ctx):
			timer.Stop()
			cancel()
			select {
			case <-stopCh:
				return
			case <-time.After(operatorsSettle):
			}
		}
	}
}

// operatorWake returns when the loop of an operator runs next: the earlier
// of next and the wish of the operator. A wish that is not after the start
// of the pass has nothing to wait for, and would run the operator every
// operatorsSettle without end; it is run again after operatorsRetry instead.
func operatorWake(next, wish, started time.Time) time.Time {
	if wish.IsZero() {
		return next
	}
	if !wish.After(started) {
		wish = started.Add(operatorsRetry)
	}
	if wish.Before(next) {
		return wish
	}
	return next
}

// operatorCount counts something an operator did, per kind.
func operatorCount(kind, what string) {
	metrics.IncrCounterWithLabels([]string{"nomad", "operator", what}, 1, []metrics.Label{{Name: "kind", Value: kind}})
}

// writeOperatorObject writes an object of a built-in operator: a changed
// copy of the stored object with a check-and-set on its modify index, or a
// new object (modify index 0). The object is validated against its kind.
func (s *Server) writeOperatorObject(obj *structs.CustomObject) (*structs.CustomObject, error) {
	snap, err := s.State().Snapshot()
	if err != nil {
		return nil, err
	}
	kind, err := snap.ObjectKindByName(nil, obj.Group, obj.Kind)
	if err != nil {
		return nil, err
	}
	if kind == nil {
		return nil, fmt.Errorf("built-in object kind %s is not registered yet", obj.KindRef())
	}

	now := time.Now().UnixNano()
	if obj.Spec, err = structs.NormalizeObjectMap(obj.Spec); err != nil {
		return nil, err
	}
	if obj.Status, err = structs.NormalizeObjectMap(obj.Status); err != nil {
		return nil, err
	}
	obj.ModifyTime = now
	operation := objectOperationStatus
	expected := obj.ModifyIndex
	if expected == 0 {
		operation = objectOperationCreate
		obj.ID = uuid.Generate()
		obj.Generation = 1
		obj.CreateTime = now
		obj.KindVersion = kind.SchemaVersion()
	}

	co := &CustomObject{srv: s}
	if err := co.validateObject(kind, obj, nil, operation, &objectWrite{args: &structs.GenericRequest{}}); err != nil {
		return nil, err
	}
	req := &structs.CustomObjectUpsertStateRequest{
		Object:              obj,
		ExpectedModifyIndex: expected,
		WriteRequest:        structs.WriteRequest{Region: s.Region()},
	}
	_, index, err := s.raftApply(structs.CustomObjectUpsertRequestType|structs.IgnoreUnknownTypeFlag, req)
	if err != nil {
		return nil, err
	}
	written := obj.Copy()
	written.ModifyIndex = index
	if expected == 0 {
		written.CreateIndex = index
	}
	return written, nil
}

// operatorEvent records an event of a built-in operator in the log of an
// object. It is best effort.
func (s *Server) operatorEvent(obj *structs.CustomObject, eventType, reason, message string) {
	if len(message) > 1000 {
		message = message[:1000]
	}
	ev := &structs.ObjectEvent{Type: eventType, Reason: reason, Message: message}
	if _, err := s.recordObjectEvents(obj, []*structs.ObjectEvent{ev}, structs.OperatorsManagedBy); err != nil {
		s.logger.Named("operators").Debug("failed to record an object event", "object", obj.KindRef()+"/"+obj.Name, "error", err)
	}
}

// objectCondition is a condition in the status of an object.
type objectCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// operatorConditions decodes the conditions of a status.
func operatorConditions(status map[string]any) []*objectCondition {
	var out []*objectCondition
	raw, _ := status[structs.ObjectStatusConditions].([]any)
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		c := &objectCondition{}
		c.Type, _ = m["type"].(string)
		c.Status, _ = m["status"].(string)
		c.Reason, _ = m["reason"].(string)
		c.Message, _ = m["message"].(string)
		c.LastTransitionTime, _ = m["lastTransitionTime"].(string)
		out = append(out, c)
	}
	return out
}

// objectConditionTrue reports whether a condition of a status is true.
func objectConditionTrue(status map[string]any, conditionType string) bool {
	for _, c := range operatorConditions(status) {
		if c.Type == conditionType {
			return c.Status == "True"
		}
	}
	return false
}

// conditionSet builds the conditions of a status from the previous ones:
// the transition time of a condition changes only when its status does.
type conditionSet struct {
	previous []*objectCondition
	now      time.Time
	out      []any
}

func newConditionSet(status map[string]any, now time.Time) *conditionSet {
	return &conditionSet{previous: operatorConditions(status), now: now}
}

// set adds a condition.
func (s *conditionSet) set(conditionType string, on bool, reason, message string) {
	status := "False"
	if on {
		status = "True"
	}
	transition := s.now.UTC().Format(time.RFC3339)
	for _, p := range s.previous {
		if p.Type == conditionType && p.Status == status && p.LastTransitionTime != "" {
			transition = p.LastTransitionTime
		}
	}
	c := map[string]any{"type": conditionType, "status": status, "lastTransitionTime": transition}
	if reason != "" {
		c["reason"] = reason
	}
	if message != "" {
		if len(message) > 1000 {
			message = message[:1000]
		}
		c["message"] = message
	}
	s.out = append(s.out, c)
}

// list returns the conditions for the status.
func (s *conditionSet) list() []any { return s.out }

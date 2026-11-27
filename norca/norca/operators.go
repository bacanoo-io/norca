// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/state"
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

	// mu guards loops and run.
	mu sync.Mutex

	// loops has a channel per operator, closed when the loop the last
	// start made for that operator has ended. The next loop of the
	// operator waits for it, so an operator makes one pass at a time
	// whatever happens to the leadership, and waits for nothing else: a
	// pass of one operator that outlives its leadership (in the call of a
	// CSI plugin, for minutes) does not hold back the others.
	loops []chan struct{}

	// run is closed when everything the last start began has ended.
	run chan struct{}
}

func newOperatorManager(srv *Server, logger hclog.Logger) *operatorManager {
	m := &operatorManager{srv: srv, logger: logger.Named("operators")}
	m.operators = []coreOperator{
		newCertificateOperator(srv, m.logger),
		newSnapshotOperator(srv, m.logger),
		newVolumeRequestOperator(srv, m.logger),
		newTenantOperator(srv, m.logger),
		newRiggerRegistryOperator(srv, m.logger),
		newRiggerOperationOperator(srv, m.logger),
	}
	return m
}

// start runs the built-in operators until stopCh is closed: when this
// server becomes the leader. It does not block. Each operator has a loop of
// its own, so one that waits for something outside (the call of a CSI plugin
// may take minutes) does not hold back the others.
func (m *operatorManager) start(stopCh chan struct{}) {
	m.mu.Lock()
	previous := m.loops
	loops := make([]chan struct{}, len(m.operators))
	for i := range loops {
		loops[i] = make(chan struct{})
	}
	run := make(chan struct{})
	m.loops, m.run = loops, run
	m.mu.Unlock()

	// The kinds are registered beside the loops: that needs the
	// leadership only, not the end of any earlier pass.
	registered := make(chan struct{})
	var all sync.WaitGroup
	all.Add(1)
	go func() {
		defer all.Done()
		if m.registerKinds(stopCh) {
			close(registered)
		}
	}()
	for i, op := range m.operators {
		all.Add(1)
		go func() {
			defer all.Done()
			defer close(loops[i])
			if previous != nil {
				// The loop the last start made for this operator: its
				// pass in flight ends on its own. It is waited for
				// even when stopCh closes meanwhile, or the start after
				// this one would not wait for that pass.
				<-previous[i]
			}
			select {
			case <-stopCh:
				return
			case <-registered:
			}
			m.operatorLoop(op, stopCh)
		}()
	}
	go func() {
		all.Wait()
		close(run)
	}()
}

// stopped returns a channel that is closed when the loops of the last
// start have ended (nil: never started).
func (m *operatorManager) stopped() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.run
}

// registerKinds registers the kinds of the built-in operators, trying again
// until it is done. It reports false when stopCh was closed first.
func (m *operatorManager) registerKinds(stopCh chan struct{}) bool {
	var kinds []*structs.ObjectKind
	for _, op := range m.operators {
		kinds = append(kinds, op.kinds()...)
	}
	for {
		select {
		case <-stopCh:
			return false
		default:
		}
		err := m.srv.registerKinds(kinds)
		if err == nil {
			return true
		}
		m.logger.Warn("registering the kinds of the built-in operators failed, will retry", "error", err)
		select {
		case <-stopCh:
			return false
		case <-time.After(operatorsRetry):
		}
	}
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

// operatorPassBudget is how long a pass of an operator that calls out goes
// on beginning with another object. The call of a CSI plugin may take
// minutes: a pass that has used its budget leaves the objects it has not
// come to for the next one, which begins with them.
const operatorPassBudget = 20 * time.Second

// operatorTurns gives the objects of an operator that calls out their turn
// across passes: a pass ends when this server is no longer the leader, or
// when it has used its budget, and the next pass begins with the object the
// last one did not come to. It belongs to one operator, and is used by its
// loop only.
type operatorTurns struct {
	srv    *Server
	budget time.Duration

	// resume is the object the next pass begins with.
	resume string
}

func operatorTurnKey(obj *structs.CustomObject) string { return obj.Namespace + "/" + obj.Name }

// order returns the objects of a pass in their turn: beginning with the one
// the last pass did not come to.
func (t *operatorTurns) order(objs []*structs.CustomObject) []*structs.CustomObject {
	at := slices.IndexFunc(objs, func(obj *structs.CustomObject) bool { return operatorTurnKey(obj) == t.resume })
	t.resume = ""
	if at > 0 {
		return append(slices.Clone(objs[at:]), objs[:at]...)
	}
	return objs
}

// next reports whether the pass that started at started goes on with the
// object at the place i of its order. It does not when this server is no
// longer the leader: a deposed leader makes no more calls. Nor when the
// pass is out of its budget, except for its first object, so every pass
// gets somewhere; again is true then, and the pass asks to run again soon.
func (t *operatorTurns) next(started time.Time, i int, obj *structs.CustomObject) (goOn, again bool) {
	if !t.srv.IsLeader() {
		return false, false
	}
	if i > 0 && time.Since(started) > t.budget {
		t.resume = operatorTurnKey(obj)
		return false, true
	}
	return true, false
}

// operatorObjectCurrent reports whether an operator may still act on an
// object it read earlier in its pass: this server is the leader, and the
// object is stored as the pass has it (the same object, not written since).
// A pass may take long (a call of a CSI plugin), and what it read at its
// start may be deleted or changed by then: an operator asks this before it
// calls out for an object, and again before every call that destroys
// something. An object that is not current is left to the next pass, which
// its change has already asked for.
func (s *Server) operatorObjectCurrent(obj *structs.CustomObject) bool {
	if !s.IsLeader() {
		return false
	}
	stored, err := s.State().CustomObjectByName(nil, obj.Group, obj.Kind, obj.Namespace, obj.Name)
	return err == nil && stored != nil && stored.ID == obj.ID && stored.ModifyIndex == obj.ModifyIndex
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
	metrics.IncrCounterWithLabels([]string{"server", "operator", what}, 1, []metrics.Label{{Name: "kind", Value: kind}})
}

// writeOperatorObject writes an object of a built-in operator: a changed
// copy of the stored object with a check-and-set on its modify index, or a
// new object (modify index 0). The object is validated against its kind.
//
// An object that is being deleted is written also when its kind refuses it
// (operatorWritePastKind; review finding ATM-24o): the check is of the whole
// object, so with a spec the kind refuses, stored before the kind said so or
// past it, no operator could write the status of the object or take its
// finalizer off, and its deletion would never end.
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
		if !operatorWritePastKind(snap, obj, err) {
			return nil, err
		}
		s.logger.Named("operators").Warn("an object that is being deleted does not pass its kind; its status and finalizers are written all the same",
			"object", obj.KindRef()+"/"+obj.Name, "namespace", obj.Namespace, "error", err)
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

// operatorWritePastKind reports whether a write of a built-in operator that
// the kind of the object refused (refusal) is made all the same: the object
// is stored, as the operator read it, with the mark of its deletion, and the
// write changes nothing of it but the status and the finalizers. So nothing
// the kind refuses is stored that is not stored already, and no changed spec
// gets past its kind this way; an object that is not being deleted is
// refused as ever. The write keeps its check-and-set on the modify index.
func operatorWritePastKind(snap *state.StateSnapshot, obj *structs.CustomObject, refusal error) bool {
	if code, _, ok := structs.CodeFromRPCCodedErr(refusal); !ok || code != http.StatusBadRequest || obj.ModifyIndex == 0 {
		return false
	}
	stored, err := snap.CustomObjectByName(nil, obj.Group, obj.Kind, obj.Namespace, obj.Name)
	if err != nil || stored == nil || !stored.Deleting() || stored.ModifyIndex != obj.ModifyIndex ||
		!structs.ObjectMapsEqual(stored.Spec, obj.Spec) {
		return false
	}
	// Everything else, but for what an operator writes.
	was, is := stored.Copy(), obj.Copy()
	for _, o := range []*structs.CustomObject{was, is} {
		o.Spec, o.Status, o.Finalizers, o.ModifyTime = nil, nil, nil, 0
	}
	return reflect.DeepEqual(was, is)
}

// operatorMessageMax is the length in bytes of the longest message of an
// event or of a condition of a built-in operator; operatorMessageCut ends
// one that was longer.
const (
	operatorMessageMax = 1000
	operatorMessageCut = " ... (truncated)"
)

// operatorMessage returns a message as an event or a condition of a built-in
// operator carries it. One that is longer than operatorMessageMax is cut,
// and says so (review finding ATM-24i): it ends with operatorMessageCut,
// within the limit, and is not cut inside a character. A message that lists
// many serial numbers or resources would else end somewhere, and nothing
// would tell its reader that a part is missing.
func operatorMessage(message string) string {
	return cutMessage(message, operatorMessageMax)
}

// cutMessage returns a message that is stored or shown with a limit of its
// length in bytes: one that is longer is cut, at a character boundary, and
// ends with operatorMessageCut, within the limit (review findings ATM-24i
// and MUK-37). The limit is longer than the mark.
func cutMessage(message string, limit int) string {
	if len(message) <= limit {
		return message
	}
	cut := max(limit-len(operatorMessageCut), 0)
	for cut > 0 && !utf8.RuneStart(message[cut]) {
		cut--
	}
	return message[:cut] + operatorMessageCut
}

// operatorEvent records an event of a built-in operator in the log of an
// object. It is best effort. A message that is too long is cut
// (operatorMessage).
func (s *Server) operatorEvent(obj *structs.CustomObject, eventType, reason, message string) {
	message = operatorMessage(message)
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

// set adds a condition. A message that is too long is cut (operatorMessage).
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
		c["message"] = operatorMessage(message)
	}
	s.out = append(s.out, c)
}

// list returns the conditions for the status.
func (s *conditionSet) list() []any { return s.out }

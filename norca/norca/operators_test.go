// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/stream"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/hashicorp/go-memdb"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// stubOperator is a built-in operator of a test: it counts its passes, and
// holds one while block is set.
type stubOperator struct {
	mu      sync.Mutex
	passes  int
	active  int
	overlap bool
	block   chan struct{}
}

func (o *stubOperator) kinds() []*structs.ObjectKind { return nil }

func (o *stubOperator) reconcile(memdb.WatchSet) (time.Time, error) {
	o.mu.Lock()
	o.passes++
	o.active++
	if o.active > 1 {
		o.overlap = true
	}
	block := o.block
	o.mu.Unlock()

	if block != nil {
		<-block
	}

	o.mu.Lock()
	o.active--
	o.mu.Unlock()
	return time.Now().Add(operatorsSettle), nil
}

func (o *stubOperator) count() (passes int, overlap bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.passes, o.overlap
}

func (o *stubOperator) hold(block chan struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.block = block
}

func waitPasses(t *testing.T, o *stubOperator, atLeast int, what string) {
	t.Helper()
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		passes, _ := o.count()
		return passes >= atLeast
	}), wait.Timeout(10*time.Second), wait.Gap(20*time.Millisecond)), must.Sprint(what))
}

// waitOperatorKinds waits until the kinds of the built-in operators of a
// server are registered. The leader registers them after it is elected, in
// a goroutine of its own (operatorManager.start), and nothing orders that
// before what a test waits for otherwise (the leader, the secret core): an
// object of such a kind written before is refused with "object kind ... not
// found" (review finding MUK-14).
func waitOperatorKinds(t *testing.T, s *Server) {
	t.Helper()
	var kinds []*structs.ObjectKind
	for _, op := range s.operators.operators {
		kinds = append(kinds, op.kinds()...)
	}
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		for _, kind := range kinds {
			if k, err := s.State().ObjectKindByName(nil, kind.Group, kind.Kind); err != nil || k == nil {
				return false
			}
		}
		return true
	}), wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond)),
		must.Sprint("the kinds of the built-in operators are registered"))
}

// A pass that outlives its leadership holds back only the next loop of its
// own operator: after the leadership is lost and gained again, the other
// operators run at once, and the operator of the pass in flight runs when
// that pass has returned, never beside it.
func TestOperators_StartWaitsPerOperator(t *testing.T) {
	ci.Parallel(t)
	s, cleanup := TestServer(t, nil)
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)

	slow, quick := &stubOperator{}, &stubOperator{}
	m := &operatorManager{srv: s, logger: s.logger.Named("test-operators"), operators: []coreOperator{slow, quick}}

	release := make(chan struct{})
	slow.hold(release)
	first := make(chan struct{})
	m.start(first)
	waitPasses(t, slow, 1, "the slow operator is in its pass")
	waitPasses(t, quick, 2, "the quick operator runs beside it")

	// Leadership lost with the pass of the slow operator in flight, and
	// gained again.
	close(first)
	firstRun := m.stopped()
	second := make(chan struct{})
	m.start(second)

	before, _ := quick.count()
	waitPasses(t, quick, before+2, "the quick operator runs under the new leadership while the old pass is in flight")
	passes, _ := slow.count()
	must.Eq(t, 1, passes, must.Sprint("the slow operator waits for its pass in flight"))
	select {
	case <-firstRun:
		t.Fatal("the first run ended while a pass was in flight")
	default:
	}

	// The old pass returns: the slow operator goes on, one pass at a time.
	slow.hold(nil)
	close(release)
	waitPasses(t, slow, 3, "the slow operator runs after its old pass returned")
	select {
	case <-firstRun:
	case <-time.After(10 * time.Second):
		t.Fatal("the first run did not end")
	}

	close(second)
	select {
	case <-m.stopped():
	case <-time.After(10 * time.Second):
		t.Fatal("the loops did not end")
	}
	for _, op := range []*stubOperator{slow, quick} {
		after, overlap := op.count()
		must.False(t, overlap, must.Sprint("two passes of one operator at a time"))
		time.Sleep(3 * operatorsSettle)
		later, _ := op.count()
		must.Eq(t, after, later, must.Sprint("a pass started after the loops ended"))
	}
}

// The turns of an operator: a pass ends between two objects when it is out
// of its budget, but not before its first one, and the next pass begins
// where it ended; a server that is not the leader goes on with nothing.
func TestOperators_Turns(t *testing.T) {
	ci.Parallel(t)
	s, cleanup := TestServer(t, nil)
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)

	objs := []*structs.CustomObject{
		{Namespace: "default", Name: "a"}, {Namespace: "default", Name: "b"}, {Namespace: "default", Name: "c"},
	}
	turns := &operatorTurns{srv: s, budget: time.Hour}
	names := func(objs []*structs.CustomObject) (out []string) {
		for _, obj := range objs {
			out = append(out, obj.Name)
		}
		return out
	}

	must.Eq(t, []string{"a", "b", "c"}, names(turns.order(objs)))
	goOn, again := turns.next(time.Now(), 1, objs[1])
	must.True(t, goOn)
	must.False(t, again)

	// Out of its budget: the first object still has its turn, the second
	// one begins the next pass.
	long := time.Now().Add(-2 * time.Hour)
	goOn, again = turns.next(long, 0, objs[0])
	must.True(t, goOn)
	must.False(t, again)
	goOn, again = turns.next(long, 1, objs[1])
	must.False(t, goOn)
	must.True(t, again)
	must.Eq(t, []string{"b", "c", "a"}, names(turns.order(objs)))
	must.Eq(t, []string{"a", "b", "c"}, names(turns.order(objs)))

	// The object is current while it is stored as it was read.
	must.False(t, s.operatorObjectCurrent(&structs.CustomObject{Group: structs.OperatorsGroup, Kind: "Nothing", Name: "x"}))

	must.NoError(t, s.Shutdown())
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool { return !s.IsLeader() }),
		wait.Timeout(10*time.Second), wait.Gap(20*time.Millisecond)))
	goOn, again = turns.next(time.Now(), 0, objs[0])
	must.False(t, goOn)
	must.False(t, again)
}

// A message of an event or of a condition that is longer than the limit is
// cut where a reader sees it (review finding ATM-24i): it ends with a mark,
// stays within the limit, and is not cut inside a character.
func TestOperatorMessage_MarkedCut(t *testing.T) {
	ci.Parallel(t)

	const limit, mark = 1000, " ... (truncated)"
	tests := []struct {
		name    string
		message string
		cut     bool
	}{
		{name: "short", message: "the certificate is revoked"},
		{name: "at the limit", message: strings.Repeat("a", limit)},
		{name: "one byte over", message: strings.Repeat("a", limit+1), cut: true},
		{name: "long", message: strings.Repeat("serial 0a1b2c3d, ", 200), cut: true},
		{name: "characters of two bytes", message: "x" + strings.Repeat("\u00e9", limit), cut: true},
		{name: "characters of four bytes", message: "xyz" + strings.Repeat("\U0001F512", limit), cut: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := newConditionSet(nil, time.Now())
			set.set("Failed", true, "Reason", tt.message)
			got, _ := set.list()[0].(map[string]any)["message"].(string)
			if !tt.cut {
				must.Eq(t, tt.message, got)
				return
			}
			must.LessEq(t, limit, len(got))
			must.True(t, utf8.ValidString(got))
			must.True(t, strings.HasSuffix(got, mark), must.Sprintf("the cut is marked: %q", got[len(got)-40:]))
			body := strings.TrimSuffix(got, mark)
			must.True(t, strings.HasPrefix(tt.message, body))
			must.Greater(t, limit-len(mark)-utf8.UTFMax, len(body), must.Sprint("nothing more is cut than the mark needs"))
		})
	}
}

// The event of an operator is cut the same way.
func TestOperatorEvent_MarkedCut(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	_, err := c.apply("", structs.CertificateIssuerKind, "internal", map[string]any{"builtin": map[string]any{}})
	must.NoError(t, err)
	issuer := c.get("", structs.CertificateIssuerKind, "internal")
	must.NotNil(t, issuer)

	long := strings.Repeat("the earlier certificate with the serial 0a1b2c3d is revoked; ", 40)
	c.s.operatorEvent(issuer, structs.ObjectEventTypeNormal, "Long", long)
	c.s.operatorEvent(issuer, structs.ObjectEventTypeNormal, "Short", "short")
	messages := c.eventMessages("", structs.CertificateIssuerKind, "internal", "Long")
	must.Len(t, 1, messages)
	must.Eq(t, 1000, len(messages[0]))
	must.True(t, strings.HasSuffix(messages[0], " ... (truncated)"), must.Sprintf("the cut is marked: %q", messages[0][960:]))
	must.Eq(t, []string{"short"}, c.eventMessages("", structs.CertificateIssuerKind, "internal", "Short"))
}

// spoilSpec changes the spec of a stored object past the check of its kind,
// as that of an object its kind came to refuse after it was stored, and
// returns the object. It checks that the object does not pass its kind any
// more.
func (c *certTest) spoilSpec(ns, kind, name string, change func(spec map[string]any)) *structs.CustomObject {
	c.t.Helper()
	must.Wait(c.t, wait.InitialSuccess(
		wait.ErrorFunc(func() error {
			spoiled := c.get(ns, kind, name).Copy()
			change(spoiled.Spec)
			resp, _, err := c.s.raftApply(structs.CustomObjectUpsertRequestType|structs.IgnoreUnknownTypeFlag, &structs.CustomObjectUpsertStateRequest{
				Object:              spoiled,
				ExpectedModifyIndex: spoiled.ModifyIndex,
				WriteRequest:        structs.WriteRequest{Region: c.s.Region()},
			})
			if respErr, ok := resp.(error); ok && err == nil {
				err = respErr
			}
			return err
		}),
		wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))
	obj := c.get(ns, kind, name)
	kindObj, err := c.s.State().ObjectKindByName(nil, obj.Group, obj.Kind)
	must.NoError(c.t, err)
	err = (&CustomObject{srv: c.s}).validateObject(kindObj, obj.Copy(), nil, objectOperationStatus, &objectWrite{args: &structs.GenericRequest{}})
	must.ErrorContains(c.t, err, "is invalid", must.Sprint("the object does not pass its kind"))
	return obj
}

// goneObject waits until an object is removed.
func (c *certTest) goneObject(ns, kind, name string) {
	c.t.Helper()
	must.Wait(c.t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return c.get(ns, kind, name) == nil }),
		wait.Timeout(30*time.Second), wait.Gap(50*time.Millisecond),
	), must.Func(func() string {
		return fmt.Sprintf("waiting for %s %s/%s to go; object: %v", kind, ns, name, c.get(ns, kind, name))
	}))
}

// An object that is being deleted can have its status and its finalizers
// written by the operators also when it does not pass its kind (review
// finding ATM-24o): the deletion would never end otherwise. Nothing else of
// it can be written that way, and an object that is not being deleted is
// refused as before.
func TestWriteOperatorObject_DeletingPastItsKind(t *testing.T) {
	ci.Parallel(t)

	c, cleanup := newCertTest(t)
	defer cleanup()
	const hold = "example.com/hold"
	for _, name := range []string{"going", "staying"} {
		_, err := c.apply("", structs.CertificateIssuerKind, name, map[string]any{"external": map[string]any{"controller": "test"}})
		must.NoError(t, err)
		withFinalizer := c.get("", structs.CertificateIssuerKind, name).Copy()
		withFinalizer.Finalizers = append(withFinalizer.Finalizers, hold)
		_, err = c.s.writeOperatorObject(withFinalizer)
		must.NoError(t, err)
		c.spoilSpec("", structs.CertificateIssuerKind, name, func(spec map[string]any) { spec["unknown"] = true })
	}
	must.NoError(t, c.delete("", structs.CertificateIssuerKind, "going"))
	going := c.get("", structs.CertificateIssuerKind, "going")
	must.NotNil(t, going)
	must.True(t, going.Deleting())

	// Not being deleted: refused as before.
	staying := c.get("", structs.CertificateIssuerKind, "staying").Copy()
	staying.Status = map[string]any{"heartbeat": "2026-11-21T10:00:00Z"}
	_, err := c.s.writeOperatorObject(staying)
	must.ErrorContains(t, err, "is invalid")

	// Being deleted: the status is written past the kind.
	changed := going.Copy()
	changed.Status = map[string]any{"heartbeat": "2026-11-21T10:00:00Z"}
	written, err := c.s.writeOperatorObject(changed)
	must.NoError(t, err, must.Sprint("the status of an object that is being deleted is written past its kind"))
	stored := c.get("", structs.CertificateIssuerKind, "going")
	must.Eq(t, written.ModifyIndex, stored.ModifyIndex)
	must.Eq(t, "2026-11-21T10:00:00Z", stored.Status["heartbeat"])
	must.Eq(t, true, stored.Spec["unknown"])

	// Nothing but the status and the finalizers.
	refused := map[string]func(obj *structs.CustomObject){
		"the spec":   func(obj *structs.CustomObject) { obj.Spec["external"] = map[string]any{"controller": "other"} },
		"the labels": func(obj *structs.CustomObject) { obj.Labels = map[string]string{"a": "b"} },
		"the owners": func(obj *structs.CustomObject) {
			obj.Owners = []*structs.ObjectOwnerReference{{Kind: "Job", Name: "x"}}
		},
		"the deletion":   func(obj *structs.CustomObject) { obj.DeletionPropagation = structs.ObjectPropagationOrphan },
		"the manager":    func(obj *structs.CustomObject) { obj.ManagedBy = "somebody" },
		"the generation": func(obj *structs.CustomObject) { obj.Generation++ },
	}
	for what, change := range refused {
		other := stored.Copy()
		other.Status = map[string]any{"heartbeat": "2026-11-21T11:00:00Z"}
		change(other)
		_, err := c.s.writeOperatorObject(other)
		must.ErrorContains(t, err, "is invalid", must.Sprint(what))
		must.Eq(t, stored.ModifyIndex, c.get("", structs.CertificateIssuerKind, "going").ModifyIndex, must.Sprint(what))
	}

	// A write of the object as it was read before is a conflict still.
	stale := going.Copy()
	stale.Status = map[string]any{"heartbeat": "2026-11-21T11:00:00Z"}
	_, err = c.s.writeOperatorObject(stale)
	must.Error(t, err)
	must.Eq(t, "2026-11-21T10:00:00Z", c.get("", structs.CertificateIssuerKind, "going").Status["heartbeat"])

	// The finalizer goes, and with it the object.
	last := stored.Copy()
	last.Finalizers = slices.DeleteFunc(last.Finalizers, func(f string) bool { return f == hold })
	written, err = c.s.writeOperatorObject(last)
	must.NoError(t, err)
	c.s.finalizeObjectIfReady(written)
	c.goneObject("", structs.CertificateIssuerKind, "going")
}

// What is stored or shown of a message that is too long is cut where its
// reader sees it (review finding MUK-37), as the messages of the operators
// are (ATM-24i): it ends with a mark, stays within its limit, and is not cut
// inside a character. Checked where such messages were cut without a sign.
func TestMessageCuts_Marked(t *testing.T) {
	ci.Parallel(t)

	const mark = " ... (truncated)"
	// Two-byte characters after an odd number of bytes: a cut at an even
	// limit falls inside one.
	long := "x" + strings.Repeat("é", 2000)
	marked := func(t *testing.T, got string, limit int) {
		t.Helper()
		must.LessEq(t, limit, len(got))
		must.True(t, utf8.ValidString(got), must.Sprint("the cut is not inside a character"))
		must.True(t, strings.HasSuffix(got, mark), must.Sprintf("the cut is marked: %q", got[max(0, len(got)-40):]))
		must.True(t, strings.HasPrefix(long, strings.TrimSuffix(got, mark)))
		must.Greater(t, limit-len(mark)-utf8.UTFMax, len(got)-len(mark), must.Sprint("nothing more is cut than the mark needs"))
	}

	t.Run("the event of a release", func(t *testing.T) {
		c, cleanup := newReleaseTestClient(t)
		defer cleanup()
		_, err := c.apply("default", "app", &structs.ReleaseApplyRequest{
			Pack: &structs.ReleasePack{Name: "app"}, Atomic: pointer.Of(false),
			Resources: []*structs.ReleaseResource{releaseNamespace("muk-ns", "v1")},
		})
		must.NoError(t, err)
		c.srv.releases.recordEvent("default", "app", structs.ObjectEventTypeNormal, "Long", long, nil)
		c.srv.releases.recordEvent("default", "app", structs.ObjectEventTypeNormal, "Short", "short", nil)
		log, err := c.srv.State().ObjectEventLog(nil, structs.ReleaseObjectGroup, structs.ReleaseKind, "default", "app")
		must.NoError(t, err)
		must.NotNil(t, log)
		found := map[string]string{}
		for _, e := range log.Events {
			found[e.Reason] = e.Message
		}
		must.Eq(t, "short", found["Short"])
		marked(t, found["Long"], 1000)
	})

	t.Run("the error of a secret access record", func(t *testing.T) {
		s, _, cleanup := secretTestServer(t, true, nil)
		defer cleanup()
		store := s.fsm.State()
		alloc := mock.Alloc()
		alloc.ClientStatus = structs.AllocClientStatusRunning
		must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, 1001, []*structs.Allocation{alloc}))
		token, _, err := s.encrypter.SignClaims(alloc.ToTaskIdentityClaims(alloc.Job, "web"))
		must.NoError(t, err)
		broker, err := store.EventBroker()
		must.NoError(t, err)
		sub, err := broker.Subscribe(&stream.SubscribeRequest{
			Topics:    map[structs.Topic][]string{structs.TopicSecretAccess: {"*"}},
			Namespace: "*",
		})
		must.NoError(t, err)
		defer sub.Unsubscribe()

		for _, text := range []string{"permission denied", long} {
			var reply structs.GenericResponse
			must.NoError(t, s.RPC(structs.SecretRecordAccessRPCMethod, &structs.SecretRecordAccessRequest{
				Record: &structs.SecretAccessEvent{Path: "kv/data/app", Provider: "vault",
					Result: structs.SecretAccessResultDenied, Error: text},
				WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: token},
			}, &reply))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var got []string
		for len(got) < 2 {
			events, err := sub.Next(ctx)
			must.NoError(t, err)
			for _, e := range events.Events {
				got = append(got, e.Payload.(*structs.SecretAccessEvent).Error)
			}
		}
		must.Eq(t, "permission denied", got[0])
		marked(t, got[1], 1024)
	})

	t.Run("the condition a tenant restore sets", func(t *testing.T) {
		message := func(text string) string {
			list := conditionsWith(nil, "2026-11-21T10:00:00Z", structs.TenantConditionRestoreFailed, true, "RestoreFailed", text)
			must.Len(t, 1, list)
			got, _ := list[0].(map[string]any)["message"].(string)
			return got
		}
		must.Eq(t, "short", message("short"))
		marked(t, message(long), 1000)
	})

	t.Run("the error of a snapshot deletion", func(t *testing.T) {
		must.Eq(t, "short", snapshotErrorText(errors.New("short")))
		marked(t, snapshotErrorText(errors.New(long)), snapshotDeleteErrorMax)
	})

	t.Run("the last failure of a volume request", func(t *testing.T) {
		c, fake, cleanup := volumeRequestTestServer(t, false)
		defer cleanup()
		fake.set(func(f *fakeVolumeCSI) { f.createErr = errors.New(long) })
		_, err := c.apply("default", csiVolumeRequestKind, "data", volumeSpec(nil))
		must.NoError(t, err)
		st := c.waitRequest("data", "failed", func(_ *structs.CustomObject, st *volumeRequestStatus) bool { return st.Failures > 0 })
		must.StrContains(t, st.LastFailure, "xéé")
		must.LessEq(t, 1000, len(st.LastFailure))
		must.True(t, utf8.ValidString(st.LastFailure), must.Sprint("the cut is not inside a character"))
		must.True(t, strings.HasSuffix(st.LastFailure, mark), must.Sprintf("the cut is marked: %q", st.LastFailure[len(st.LastFailure)-40:]))
	})
}

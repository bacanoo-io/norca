// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bacanoo-io/norca/norca/ci"
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

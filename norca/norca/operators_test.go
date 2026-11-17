// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"sync"
	"testing"
	"time"

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

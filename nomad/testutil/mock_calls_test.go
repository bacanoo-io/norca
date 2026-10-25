// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package testutil

import (
	"fmt"
	"sync"
	"testing"

	"github.com/hashicorp/nomad/ci"
	testinginterface "github.com/mitchellh/go-testing-interface"
	"github.com/shoenig/test/must"
)

// recordingT is a go-testing-interface T that records failures instead of
// failing the real test.
type recordingT struct {
	testinginterface.RuntimeT
	errors []string
}

func (r *recordingT) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recordingT) Helper() {}

func TestCallCounter_IncGet(t *testing.T) {
	ci.Parallel(t)

	c := NewCallCounter()
	must.MapEmpty(t, c.Get())

	c.Inc("a")
	c.Inc("b")
	c.Inc("a")
	must.MapEq(t, map[string]int{"a": 2, "b": 1}, c.Get())
}

func TestCallCounter_GetReturnsCopy(t *testing.T) {
	ci.Parallel(t)

	c := NewCallCounter()
	c.Inc("a")

	got := c.Get()
	got["a"] = 100
	got["z"] = 1

	must.MapEq(t, map[string]int{"a": 1}, c.Get())
}

func TestCallCounter_ZeroValue(t *testing.T) {
	ci.Parallel(t)

	var c CallCounter
	must.MapEmpty(t, c.Get())
	c.Inc("a")
	must.MapEq(t, map[string]int{"a": 1}, c.Get())
}

func TestCallCounter_Concurrent(t *testing.T) {
	ci.Parallel(t)

	c := NewCallCounter()
	const workers, perWorker = 8, 500

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				c.Inc("shared")
				c.Inc(fmt.Sprintf("worker-%d", i))
				_ = c.Get()
			}
		}(i)
	}
	wg.Wait()

	got := c.Get()
	must.Eq(t, workers*perWorker, got["shared"])
	for i := 0; i < workers; i++ {
		must.Eq(t, perWorker, got[fmt.Sprintf("worker-%d", i)])
	}
}

func TestCallCounter_AssertCalled(t *testing.T) {
	ci.Parallel(t)

	c := NewCallCounter()
	c.Inc("called")

	rt := &recordingT{}
	c.AssertCalled(rt, "called")
	must.SliceEmpty(t, rt.errors)

	c.AssertCalled(rt, "never-called")
	must.Len(t, 1, rt.errors)
	must.StrContains(t, rt.errors[0], "never-called")
}

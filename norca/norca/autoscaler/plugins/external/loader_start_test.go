// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package external

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/autoscaler/plugins/external/externaltest"
	"github.com/shoenig/test/must"
)

// A start of the process does not hold the instance: the callers that need it
// meanwhile wait for that one start, and a close returns at once; the process
// of a start that ends after the close is stopped (review finding ASP-8).
func TestLoader_StartWithoutTheLock(t *testing.T) {
	ci.Parallel(t)
	dir := t.TempDir()
	externaltest.InstallPlugin(t, dir, "test-apm")

	l, clock := newTestLoader(t, dir)
	apm, err := l.APM("my-apm", "test-apm", nil, nil)
	must.NoError(t, err)
	inst := apm.(*apmInstance).instance

	// The process exits: the next calls start it again, after the backoff.
	inst.l.Lock()
	inst.client.Kill()
	inst.l.Unlock()
	_, err = apm.Query("1", testRange())
	must.ErrorContains(t, err, "next start attempt")
	clock.Advance(time.Hour)

	var starts atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	l.beforeLaunch = func() {
		if starts.Add(1) == 1 {
			close(entered)
		}
		<-release
	}

	errs := make(chan error, 3)
	for range 3 {
		go func() {
			_, err := apm.Query("1", testRange())
			errs <- err
		}()
	}
	<-entered

	// The configuration is stored while the start runs.
	done := make(chan struct{})
	go func() {
		inst.l.Lock()
		_ = inst.config
		inst.l.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the instance is locked while its process starts")
	}

	close(release)
	for range 3 {
		must.NoError(t, <-errs)
	}
	must.Eq(t, int32(1), starts.Load())

	// A close during a start returns at once; the started process is stopped.
	inst.l.Lock()
	inst.client.Kill()
	inst.l.Unlock()
	_, err = apm.Query("1", testRange())
	must.ErrorContains(t, err, "next start attempt")
	clock.Advance(time.Hour)
	starts.Store(0)
	entered, release = make(chan struct{}), make(chan struct{})
	go func() {
		_, err := apm.Query("1", testRange())
		errs <- err
	}()
	<-entered
	closed := make(chan struct{})
	go func() {
		inst.close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("close waited for the start")
	}
	close(release)
	must.ErrorIs(t, <-errs, ErrShutdown)
	inst.l.Lock()
	defer inst.l.Unlock()
	must.Nil(t, inst.client)
}

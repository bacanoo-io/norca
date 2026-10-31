// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package docker

import (
	"context"
	"testing"
	"time"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

func TestStopTimeoutSeconds(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		in  time.Duration
		exp uint
	}{
		{0, 0},
		{-time.Second, 0},
		{time.Millisecond, 1},
		{500 * time.Millisecond, 1},
		{time.Second, 1},
		{5900 * time.Millisecond, 6},
		{30 * time.Second, 30},
	}
	for _, c := range cases {
		must.Eq(t, c.exp, stopTimeoutSeconds(c.in), must.Sprintf("kill timeout %v", c.in))
	}
}

func TestAuthIsEmpty(t *testing.T) {
	ci.Parallel(t)

	must.True(t, authIsEmpty(nil))
	must.True(t, authIsEmpty(&docker.AuthConfiguration{}))
	must.False(t, authIsEmpty(&docker.AuthConfiguration{Username: "u"}))
	must.False(t, authIsEmpty(&docker.AuthConfiguration{ServerAddress: "r"}))
	must.False(t, authIsEmpty(&docker.AuthConfiguration{IdentityToken: "t"}))
	must.False(t, authIsEmpty(&docker.AuthConfiguration{RegistryToken: "t"}))
}

func TestBindWithLabel(t *testing.T) {
	ci.Parallel(t)

	must.Eq(t, "/a:/b", bindWithLabel("/a:/b", "", ""))
	must.Eq(t, "/a:/b:ro", bindWithLabel("/a:/b", "ro", ""))
	must.Eq(t, "/a:/b:z", bindWithLabel("/a:/b", "", "z"))
	must.Eq(t, "/a:/b:ro,z", bindWithLabel("/a:/b", "ro", "z"))
}

func TestPullKey(t *testing.T) {
	ci.Parallel(t)

	must.Eq(t, "foo", pullKey("foo", nil))
	must.Eq(t, "foo", pullKey("foo", &docker.AuthConfiguration{}))

	a := pullKey("foo", &docker.AuthConfiguration{Username: "a", Password: "x"})
	b := pullKey("foo", &docker.AuthConfiguration{Username: "b", Password: "x"})
	must.NotEq(t, "foo", a)
	must.NotEq(t, a, b)
	must.Eq(t, a, pullKey("foo", &docker.AuthConfiguration{Username: "a", Password: "x"}))
	must.StrNotContains(t, a, "x")
}

// TestDockerCoordinator_PullImage_KeepsNewerFuture asserts a waiter does not
// delete a future registered for the same image after its own pull finished.
func TestDockerCoordinator_PullImage_KeepsNewerFuture(t *testing.T) {
	ci.Parallel(t)

	image := "foo"
	coordinator := newDockerCoordinator(&dockerCoordinatorConfig{
		ctx:    context.Background(),
		logger: testlog.HCLogger(t),
		client: newMockImageClient(map[string]string{}, 0),
	})

	// a pull already in flight, joined by the caller below
	old := newPullFuture()
	coordinator.pullFutures[image] = old

	done := make(chan struct{})
	go func() {
		defer close(done)
		coordinator.PullImage(image, nil, uuid.Generate(), noopLogEventFn, time.Minute, time.Minute)
	}()

	testutil.WaitForResult(func() (bool, error) {
		coordinator.pullLoggerLock.RLock()
		defer coordinator.pullLoggerLock.RUnlock()
		return len(coordinator.pullLoggers[image]) == 1, nil
	}, func(err error) {
		t.Fatal("caller did not join the pull")
	})

	// finish the old pull and, before the caller wakes, have a new pull of the
	// same image registered
	newer := newPullFuture()
	coordinator.imageLock.Lock()
	old.set("id", nil)
	coordinator.pullFutures[image] = newer
	coordinator.imageLock.Unlock()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("PullImage did not return")
	}

	coordinator.imageLock.Lock()
	defer coordinator.imageLock.Unlock()
	must.Eq(t, newer, coordinator.pullFutures[image])
}

// TestDriver_WaitTask_NoReader asserts the wait goroutine finishes even when
// the caller never reads the result.
func TestDriver_WaitTask_NoReader(t *testing.T) {
	ci.Parallel(t)

	d := &Driver{tasks: newTaskStore()}
	h := &taskHandle{waitCh: make(chan struct{})}
	d.tasks.Set("task", h)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := d.WaitTask(ctx, "task")
	must.NoError(t, err)
	cancel()

	// the result is buffered and the channel closed without any reader
	testutil.WaitForResult(func() (bool, error) {
		return len(ch) == 1, nil
	}, func(err error) {
		t.Fatal("wait result was not buffered")
	})
	res := <-ch
	must.ErrorIs(t, res.Err, context.Canceled)
	_, ok := <-ch
	must.False(t, ok)
}

// TestDriver_DockerStatsCollector_EarlyTimer asserts stats are still emitted
// when the interval timer fires before docker sent the first stats.
func TestDriver_DockerStatsCollector_EarlyTimer(t *testing.T) {
	ci.Parallel(t)

	src := make(chan *docker.Stats)
	defer close(src)
	dst, recvCh := newStatsChanPipe()
	defer dst.close()

	go dockerStatsCollector(dst, src, time.Millisecond)

	// let the timer fire with no stats received yet
	time.Sleep(50 * time.Millisecond)
	src <- &docker.Stats{}

	select {
	case ru := <-recvCh:
		must.NotNil(t, ru)
	case <-time.After(5 * time.Second):
		t.Fatal("no stats emitted after the timer fired early")
	}
}

// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package volumewatcher

import (
	"sync"
	"testing"
	"time"

	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
	"github.com/stretchr/testify/require"
)

// TestVolumeWatch_EnableDisable tests the watcher registration logic that needs
// to happen during leader step-up/step-down
func TestVolumeWatch_EnableDisable(t *testing.T) {
	ci.Parallel(t)

	srv := &MockRPCServer{}
	srv.state = state.TestStateStore(t)
	index := uint64(100)

	watcher := NewVolumesWatcher(testlog.HCLogger(t), srv, "")
	watcher.quiescentTimeout = 100 * time.Millisecond
	watcher.SetEnabled(true, srv.State(), "")

	plugin := mock.CSIPlugin()
	node := testNode(plugin, srv.State())
	alloc := mock.Alloc()
	alloc.ClientStatus = structs.AllocClientStatusComplete

	vol := testVolume(plugin, alloc, node.ID)

	index++
	err := srv.State().UpsertCSIVolume(index, []*structs.CSIVolume{vol})
	require.NoError(t, err)

	// need to have just enough of a volume and claim in place so that
	// the watcher doesn't immediately stop and unload itself
	claim := &structs.CSIVolumeClaim{
		Mode:  structs.CSIVolumeClaimGC,
		State: structs.CSIVolumeClaimStateNodeDetached,
	}
	index++
	err = srv.State().CSIVolumeClaim(index, vol.Namespace, vol.ID, claim)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		watcher.wlock.RLock()
		defer watcher.wlock.RUnlock()
		return 1 == len(watcher.watchers)
	}, time.Second, 10*time.Millisecond)

	watcher.SetEnabled(false, nil, "")
	watcher.wlock.RLock()
	defer watcher.wlock.RUnlock()
	require.Equal(t, 0, len(watcher.watchers))
}

// TestVolumeWatch_LeadershipTransition tests the correct behavior of
// claim reaping across leader step-up/step-down
func TestVolumeWatch_LeadershipTransition(t *testing.T) {
	ci.Parallel(t)

	srv := &MockRPCServer{}
	srv.state = state.TestStateStore(t)
	index := uint64(100)

	watcher := NewVolumesWatcher(testlog.HCLogger(t), srv, "")
	watcher.quiescentTimeout = 100 * time.Millisecond

	plugin := mock.CSIPlugin()
	node := testNode(plugin, srv.State())
	alloc := mock.Alloc()
	alloc.ClientStatus = structs.AllocClientStatusRunning
	vol := testVolume(plugin, alloc, node.ID)

	index++
	err := srv.State().UpsertAllocs(structs.MsgTypeTestSetup, index,
		[]*structs.Allocation{alloc})
	require.NoError(t, err)

	watcher.SetEnabled(true, srv.State(), "")

	index++
	err = srv.State().UpsertCSIVolume(index, []*structs.CSIVolume{vol})
	require.NoError(t, err)

	// we should get or start up a watcher when we get an update for
	// the volume from the state store
	require.Eventually(t, func() bool {
		watcher.wlock.RLock()
		defer watcher.wlock.RUnlock()
		return 1 == len(watcher.watchers)
	}, time.Second, 10*time.Millisecond)

	vol, _ = srv.State().CSIVolumeByID(nil, vol.Namespace, vol.ID)
	require.Len(t, vol.PastClaims, 0, "expected to have 0 PastClaims")
	require.Equal(t, int64(0), srv.countCSIUnpublish.Load(), "expected no CSI.Unpublish RPC calls")

	// trying to test a dropped watch is racy, so to reliably simulate
	// this condition, step-down the watcher first and then perform
	// the writes to the volume before starting the new watcher. no
	// watches for that change will fire on the new watcher

	// step-down (this is sync)
	watcher.SetEnabled(false, nil, "")
	watcher.wlock.RLock()
	require.Equal(t, 0, len(watcher.watchers))
	watcher.wlock.RUnlock()

	// allocation is now invalid
	index++
	err = srv.State().DeleteEval(index, []string{}, []string{alloc.ID}, false)
	require.NoError(t, err)

	// emit a GC so that we have a volume change that's dropped
	claim := &structs.CSIVolumeClaim{
		AllocationID: alloc.ID,
		NodeID:       node.ID,
		Mode:         structs.CSIVolumeClaimGC,
		State:        structs.CSIVolumeClaimStateUnpublishing,
	}
	index++
	err = srv.State().CSIVolumeClaim(index, vol.Namespace, vol.ID, claim)
	require.NoError(t, err)

	// create a new watcher and enable it to simulate the leadership
	// transition
	watcher = NewVolumesWatcher(testlog.HCLogger(t), srv, "")
	watcher.quiescentTimeout = 100 * time.Millisecond
	watcher.SetEnabled(true, srv.State(), "")

	// wait for the new watcher to reap the claim and then stop; checking
	// only for zero watchers could succeed before the watcher has started
	require.Eventually(t, func() bool {
		watcher.wlock.RLock()
		defer watcher.wlock.RUnlock()
		return 1 == srv.countCSIUnpublish.Load() && 0 == len(watcher.watchers)
	}, time.Second, 10*time.Millisecond)

	vol, _ = srv.State().CSIVolumeByID(nil, vol.Namespace, vol.ID)
	require.Len(t, vol.PastClaims, 1, "expected to have 1 PastClaim")
	require.Equal(t, int64(1), srv.countCSIUnpublish.Load(), "expected CSI.Unpublish RPC to be called")
}

// TestVolumeWatch_StartStop tests the start and stop of the watcher when
// it receives notifcations and has completed its work
func TestVolumeWatch_StartStop(t *testing.T) {
	ci.Parallel(t)

	srv := &MockStatefulRPCServer{}
	srv.state = state.TestStateStore(t)
	index := uint64(100)
	watcher := NewVolumesWatcher(testlog.HCLogger(t), srv, "")
	watcher.quiescentTimeout = 100 * time.Millisecond

	watcher.SetEnabled(true, srv.State(), "")
	require.Equal(t, 0, len(watcher.watchers))

	plugin := mock.CSIPlugin()
	node := testNode(plugin, srv.State())
	alloc1 := mock.Alloc()
	alloc1.ClientStatus = structs.AllocClientStatusRunning
	alloc2 := mock.Alloc()
	alloc2.Job = alloc1.Job
	alloc2.ClientStatus = structs.AllocClientStatusRunning
	index++
	err := srv.State().UpsertJob(structs.MsgTypeTestSetup, index, nil, alloc1.Job)
	require.NoError(t, err)
	index++
	err = srv.State().UpsertAllocs(structs.MsgTypeTestSetup, index, []*structs.Allocation{alloc1, alloc2})
	require.NoError(t, err)

	// register a volume and an unused volume
	vol := testVolume(plugin, alloc1, node.ID)
	index++
	err = srv.State().UpsertCSIVolume(index, []*structs.CSIVolume{vol})
	require.NoError(t, err)

	// assert we get a watcher; there are no claims so it should immediately stop
	require.Eventually(t, func() bool {
		watcher.wlock.RLock()
		defer watcher.wlock.RUnlock()
		return 0 == len(watcher.watchers)
	}, time.Second*2, 10*time.Millisecond)

	// claim the volume for both allocs
	claim := &structs.CSIVolumeClaim{
		AllocationID: alloc1.ID,
		NodeID:       node.ID,
		Mode:         structs.CSIVolumeClaimRead,
		AccessMode:   structs.CSIVolumeAccessModeMultiNodeReader,
	}

	index++
	err = srv.State().CSIVolumeClaim(index, vol.Namespace, vol.ID, claim)
	require.NoError(t, err)
	claim.AllocationID = alloc2.ID
	index++
	err = srv.State().CSIVolumeClaim(index, vol.Namespace, vol.ID, claim)
	require.NoError(t, err)

	// reap the volume and assert nothing has happened
	claim = &structs.CSIVolumeClaim{
		AllocationID: alloc1.ID,
		NodeID:       node.ID,
	}
	index++
	err = srv.State().CSIVolumeClaim(index, vol.Namespace, vol.ID, claim)
	require.NoError(t, err)

	ws := memdb.NewWatchSet()
	vol, _ = srv.State().CSIVolumeByID(ws, vol.Namespace, vol.ID)
	require.Equal(t, 2, len(vol.ReadAllocs))

	// alloc becomes terminal
	alloc1 = alloc1.Copy()
	alloc1.ClientStatus = structs.AllocClientStatusComplete
	index++
	err = srv.State().UpsertAllocs(structs.MsgTypeTestSetup, index, []*structs.Allocation{alloc1})
	require.NoError(t, err)
	index++
	claim.State = structs.CSIVolumeClaimStateReadyToFree
	err = srv.State().CSIVolumeClaim(index, vol.Namespace, vol.ID, claim)
	require.NoError(t, err)

	// watcher stops and 1 claim has been released
	require.Eventually(t, func() bool {
		watcher.wlock.RLock()
		defer watcher.wlock.RUnlock()
		return 0 == len(watcher.watchers)
	}, time.Second*5, 10*time.Millisecond)

	vol, _ = srv.State().CSIVolumeByID(ws, vol.Namespace, vol.ID)
	must.Eq(t, 1, len(vol.ReadAllocs))
	must.Eq(t, 0, len(vol.PastClaims))
}

// TestVolumeWatch_Delete tests the stop of the watcher when it receives
// notifications around a deleted volume
func TestVolumeWatch_Delete(t *testing.T) {
	ci.Parallel(t)

	srv := &MockStatefulRPCServer{}
	srv.state = state.TestStateStore(t)
	index := uint64(100)
	watcher := NewVolumesWatcher(testlog.HCLogger(t), srv, "")
	watcher.quiescentTimeout = 100 * time.Millisecond

	watcher.SetEnabled(true, srv.State(), "")
	must.Eq(t, 0, len(watcher.watchers))

	// register an unused volume
	plugin := mock.CSIPlugin()
	vol := mock.CSIVolume(plugin)
	index++
	must.NoError(t, srv.State().UpsertCSIVolume(index, []*structs.CSIVolume{vol}))

	// assert we get a watcher; there are no claims so it should immediately stop
	require.Eventually(t, func() bool {
		watcher.wlock.RLock()
		defer watcher.wlock.RUnlock()
		return 0 == len(watcher.watchers)
	}, time.Second*2, 10*time.Millisecond)

	// write a GC claim to the volume and then immediately delete, to
	// potentially hit the race condition between updates and deletes
	index++
	must.NoError(t, srv.State().CSIVolumeClaim(index, vol.Namespace, vol.ID,
		&structs.CSIVolumeClaim{
			Mode:  structs.CSIVolumeClaimGC,
			State: structs.CSIVolumeClaimStateReadyToFree,
		}))

	index++
	must.NoError(t, srv.State().CSIVolumeDeregister(
		index, vol.Namespace, []string{vol.ID}, false))

	// the watcher should not be running
	require.Eventually(t, func() bool {
		watcher.wlock.RLock()
		defer watcher.wlock.RUnlock()
		return 0 == len(watcher.watchers)
	}, time.Second*5, 10*time.Millisecond)

}

// TestVolumeWatch_RegisterDeregister tests the start and stop of
// watchers around registration
func TestVolumeWatch_RegisterDeregister(t *testing.T) {
	ci.Parallel(t)

	srv := &MockStatefulRPCServer{}
	srv.state = state.TestStateStore(t)

	index := uint64(100)

	watcher := NewVolumesWatcher(testlog.HCLogger(t), srv, "")
	watcher.quiescentTimeout = 10 * time.Millisecond

	watcher.SetEnabled(true, srv.State(), "")
	require.Equal(t, 0, len(watcher.watchers))

	plugin := mock.CSIPlugin()
	alloc := mock.Alloc()
	alloc.ClientStatus = structs.AllocClientStatusComplete

	// register a volume without claims
	vol := mock.CSIVolume(plugin)
	index++
	err := srv.State().UpsertCSIVolume(index, []*structs.CSIVolume{vol})
	require.NoError(t, err)

	// watcher should stop
	require.Eventually(t, func() bool {
		watcher.wlock.RLock()
		defer watcher.wlock.RUnlock()
		return 0 == len(watcher.watchers)
	}, time.Second, 10*time.Millisecond)
}

// blockingRPCServer blocks the first Unpublish RPC until release is
// closed, to simulate a slow client RPC
type blockingRPCServer struct {
	MockRPCServer
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func newBlockingRPCServer(t *testing.T) *blockingRPCServer {
	srv := &blockingRPCServer{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	srv.state = state.TestStateStore(t)
	return srv
}

func (srv *blockingRPCServer) Unpublish(args *structs.CSIVolumeUnpublishRequest, reply *structs.CSIVolumeUnpublishResponse) error {
	first := false
	srv.once.Do(func() { first = true })
	if first {
		close(srv.entered)
		<-srv.release
	}
	return srv.MockRPCServer.Unpublish(args, reply)
}

// upsertVolumeWithPastClaim registers a volume claimed by an allocation
// that doesn't exist, so the volume watcher has a claim to unpublish
func upsertVolumeWithPastClaim(t *testing.T, s *state.StateStore, index uint64) *structs.CSIVolume {
	plugin := mock.CSIPlugin()
	node := testNode(plugin, s)
	vol := testVolume(plugin, mock.Alloc(), node.ID)
	must.NoError(t, s.UpsertCSIVolume(index, []*structs.CSIVolume{vol}))
	return vol
}

// TestVolumeWatch_NotifyDuringSlowRPC asserts that updates for a volume
// whose watcher is stuck in an Unpublish RPC don't block the volumes
// watcher (which holds its lock while notifying) or the leader step-down
func TestVolumeWatch_NotifyDuringSlowRPC(t *testing.T) {
	ci.Parallel(t)

	srv := newBlockingRPCServer(t)
	t.Cleanup(func() { close(srv.release) })

	watcher := NewVolumesWatcher(testlog.HCLogger(t), srv, "")
	watcher.quiescentTimeout = time.Hour
	vol := upsertVolumeWithPastClaim(t, srv.State(), 100)
	watcher.SetEnabled(true, srv.State(), "")

	select {
	case <-srv.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher never called Unpublish")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 3; i++ {
			must.NoError(t, watcher.add(vol))
		}
		watcher.SetEnabled(false, nil, "")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("volume update or step-down blocked behind an in-flight Unpublish RPC")
	}
}

// TestVolumeWatch_StaleWatcherAfterLeadershipChange asserts that a
// watcher from a previous leadership term that finishes late doesn't
// remove the watcher of the current term for the same volume
func TestVolumeWatch_StaleWatcherAfterLeadershipChange(t *testing.T) {
	ci.Parallel(t)

	srv := newBlockingRPCServer(t)
	watcher := NewVolumesWatcher(testlog.HCLogger(t), srv, "")
	watcher.quiescentTimeout = time.Hour
	upsertVolumeWithPastClaim(t, srv.State(), 100)
	watcher.SetEnabled(true, srv.State(), "")

	select {
	case <-srv.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher never called Unpublish")
	}

	// step down while the old watcher is in the RPC, then step up again
	watcher.SetEnabled(false, nil, "")
	watcher.SetEnabled(true, srv.State(), "")
	t.Cleanup(func() { watcher.SetEnabled(false, nil, "") })

	numWatchers := func() int {
		watcher.wlock.RLock()
		defer watcher.wlock.RUnlock()
		return len(watcher.watchers)
	}
	require.Eventually(t, func() bool {
		return numWatchers() == 1 && srv.countCSIUnpublish.Load() == 1
	}, 5*time.Second, 10*time.Millisecond)

	// let the old watcher's RPC finish; it must exit without touching
	// the new term's watchers
	close(srv.release)
	require.Eventually(t, func() bool {
		return srv.countCSIUnpublish.Load() == 2
	}, 5*time.Second, 10*time.Millisecond)
	require.Never(t, func() bool { return numWatchers() != 1 },
		500*time.Millisecond, 10*time.Millisecond,
		"old watcher removed the new leader's watcher")
}

// TestVolumeWatch_Remove asserts that a volume watcher only leaves the
// watch list when no update is queued for it, and never removes another
// watcher stored under its key
func TestVolumeWatch_Remove(t *testing.T) {
	ci.Parallel(t)

	srv := &MockRPCServer{}
	srv.state = state.TestStateStore(t)
	watcher := NewVolumesWatcher(testlog.HCLogger(t), srv, "")
	watcher.SetEnabled(true, srv.State(), "")
	t.Cleanup(func() { watcher.SetEnabled(false, nil, "") })

	vol := mock.CSIVolume(mock.CSIPlugin())
	key := structs.NamespacedID{ID: vol.ID, Namespace: vol.Namespace}
	numWatchers := func() int {
		watcher.wlock.RLock()
		defer watcher.wlock.RUnlock()
		return len(watcher.watchers)
	}

	// a watcher without a goroutine, so the test controls updateCh
	vw := &volumeWatcher{updateCh: make(chan *structs.CSIVolume, 1)}
	watcher.wlock.Lock()
	watcher.watchers[key] = vw
	vw.Notify(vol)
	vw.Notify(vol) // must not block
	watcher.wlock.Unlock()

	must.False(t, watcher.remove(key, vw), must.Sprint("update queued"))
	must.Eq(t, 1, numWatchers())

	<-vw.updateCh
	must.True(t, watcher.remove(key, vw))
	must.Eq(t, 0, numWatchers())

	other := &volumeWatcher{updateCh: make(chan *structs.CSIVolume, 1)}
	watcher.wlock.Lock()
	watcher.watchers[key] = other
	watcher.wlock.Unlock()
	must.True(t, watcher.remove(key, vw))
	must.Eq(t, 1, numWatchers())
}

// TestVolumeWatch_NamespacedKeys asserts that volumes whose ID and
// namespace concatenate to the same string get their own watchers
func TestVolumeWatch_NamespacedKeys(t *testing.T) {
	ci.Parallel(t)

	srv := &MockRPCServer{}
	srv.state = state.TestStateStore(t)
	ns := mock.Namespace()
	ns.Name = "ault"
	must.NoError(t, srv.State().UpsertNamespaces(100, []*structs.Namespace{ns}))

	plugin := mock.CSIPlugin()
	vol1 := mock.CSIVolume(plugin)
	vol1.ID = "vol"
	vol2 := mock.CSIVolume(plugin)
	vol2.ID = "voldef"
	vol2.Namespace = "ault"
	must.NoError(t, srv.State().UpsertCSIVolume(101, []*structs.CSIVolume{vol1, vol2}))

	watcher := NewVolumesWatcher(testlog.HCLogger(t), srv, "")
	watcher.quiescentTimeout = time.Hour
	watcher.SetEnabled(true, srv.State(), "")
	t.Cleanup(func() { watcher.SetEnabled(false, nil, "") })

	require.Eventually(t, func() bool {
		watcher.wlock.RLock()
		defer watcher.wlock.RUnlock()
		return len(watcher.watchers) == 2
	}, 5*time.Second, 10*time.Millisecond)
}

// TestVolumeWatch_RapidLeadershipChanges toggles leadership while
// volumes change, for the race detector
func TestVolumeWatch_RapidLeadershipChanges(t *testing.T) {
	ci.Parallel(t)

	srv := &MockRPCServer{}
	srv.state = state.TestStateStore(t)
	watcher := NewVolumesWatcher(testlog.HCLogger(t), srv, "")
	watcher.quiescentTimeout = 10 * time.Millisecond

	plugin := mock.CSIPlugin()
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		for index := uint64(100); ; index++ {
			select {
			case <-stopCh:
				return
			default:
			}
			vol := mock.CSIVolume(plugin)
			if err := srv.State().UpsertCSIVolume(index, []*structs.CSIVolume{vol}); err != nil {
				t.Error(err)
				return
			}
		}
	}()

	for i := 0; i < 200; i++ {
		watcher.SetEnabled(true, srv.State(), "")
		watcher.SetEnabled(false, nil, "")
	}
	close(stopCh)
	<-doneCh
}

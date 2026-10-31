// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package volumewatcher

import (
	"context"
	"sync"
	"time"

	log "github.com/hashicorp/go-hclog"
	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// Watcher is used to watch volumes and their allocations created
// by the scheduler and trigger the scheduler when allocation health
// transitions.
type Watcher struct {
	enabled bool
	logger  log.Logger

	// rpc contains the set of Server methods that can be used by
	// the volumes watcher for RPC
	rpc CSIVolumeRPC

	// the ACL needed to send RPCs
	leaderAcl string

	// state is the state that is watched for state changes.
	state *state.StateStore

	// watchers is the set of active watchers, one per volume
	//
	// white room bugfix: the key was vol.ID+vol.Namespace, so volume "vol"
	// in namespace "default" and volume "voldef" in namespace "ault"
	// shared one watcher (volume IDs are free-form)
	watchers map[structs.NamespacedID]*volumeWatcher

	// ctx and exitFn are used to cancel the watcher
	ctx    context.Context
	exitFn context.CancelFunc

	// quiescentTimeout is the time we wait until the volume has "settled"
	// before stopping the child watcher goroutines
	quiescentTimeout time.Duration

	wlock sync.RWMutex
}

var defaultQuiescentTimeout = time.Minute * 5

// NewVolumesWatcher returns a volumes watcher that is used to watch
// volumes and trigger the scheduler as needed.
func NewVolumesWatcher(logger log.Logger, rpc CSIVolumeRPC, leaderAcl string) *Watcher {

	// the leader step-down calls SetEnabled(false) which is what
	// cancels this context, rather than passing in its own shutdown
	// context
	ctx, exitFn := context.WithCancel(context.Background())

	return &Watcher{
		rpc:              rpc,
		logger:           logger.Named("volumes_watcher"),
		ctx:              ctx,
		exitFn:           exitFn,
		leaderAcl:        leaderAcl,
		quiescentTimeout: defaultQuiescentTimeout,
	}
}

// SetEnabled is used to control if the watcher is enabled. The
// watcher should only be enabled on the active leader. When being
// enabled the state and leader's ACL is passed in as it is no longer
// valid once a leader election has taken place.
func (w *Watcher) SetEnabled(enabled bool, state *state.StateStore, leaderAcl string) {
	w.wlock.Lock()
	defer w.wlock.Unlock()

	wasEnabled := w.enabled
	w.enabled = enabled
	w.leaderAcl = leaderAcl

	if state != nil {
		w.state = state
	}

	// Flush the state to create the necessary objects
	w.flush(enabled)

	// If we are starting now, launch the watch daemon
	//
	// white room bugfix: the daemon read w.state without the lock, racing
	// with the write above when a daemon from the previous leadership term
	// had not yet seen its context cancelled; it now gets its state store
	if enabled && !wasEnabled {
		go w.watchVolumes(w.ctx, w.state)
	}
}

// flush is used to clear the state of the watcher
func (w *Watcher) flush(enabled bool) {
	// Kill everything associated with the watcher; cancelling the context
	// stops all the volume watchers
	if w.exitFn != nil {
		w.exitFn()
	}

	w.watchers = make(map[structs.NamespacedID]*volumeWatcher, 32)
	w.ctx, w.exitFn = context.WithCancel(context.Background())
}

// getVolumesRetryInterval is how long watchVolumes waits after a failed
// query of the volumes
const getVolumesRetryInterval = time.Second

// watchVolumes is the long lived go-routine that watches for volumes to
// add and remove watchers on.
func (w *Watcher) watchVolumes(ctx context.Context, state *state.StateStore) {
	vIndex := uint64(1)
	for {
		volumes, idx, err := w.getVolumes(ctx, state, vIndex)
		if err != nil {
			if err == context.Canceled {
				return
			}
			w.logger.Error("failed to retrieve volumes", "error", err)

			// white room bugfix: on error the loop retried at once and
			// reset the index to 0, which makes the next query
			// non-blocking, so a persistent error spun the CPU and
			// flooded the log. Wait and keep the last-seen index
			timer, stop := helper.NewSafeTimer(getVolumesRetryInterval)
			select {
			case <-ctx.Done():
				stop()
				return
			case <-timer.C:
			}
			stop()
			continue
		}

		vIndex = idx // last-seen index
		for _, v := range volumes {
			if err := w.add(v); err != nil {
				w.logger.Error("failed to track volume", "volume_id", v.ID, "error", err)
			}

		}
	}
}

// getVolumes retrieves all volumes blocking at the given index.
func (w *Watcher) getVolumes(ctx context.Context, state *state.StateStore, minIndex uint64) ([]*structs.CSIVolume, uint64, error) {
	resp, index, err := state.BlockingQuery(w.getVolumesImpl, minIndex, ctx)
	if err != nil {
		return nil, 0, err
	}

	return resp.([]*structs.CSIVolume), index, nil
}

// getVolumesImpl retrieves all volumes from the passed state store.
func (w *Watcher) getVolumesImpl(ws memdb.WatchSet, state *state.StateStore) (interface{}, uint64, error) {

	iter, err := state.CSIVolumes(ws)
	if err != nil {
		return nil, 0, err
	}

	var volumes []*structs.CSIVolume
	for {
		raw := iter.Next()
		if raw == nil {
			break
		}
		volume := raw.(*structs.CSIVolume)
		volumes = append(volumes, volume)
	}

	// Use the last index that affected the volume table
	index, err := state.Index("csi_volumes")
	if err != nil {
		return nil, 0, err
	}

	return volumes, index, nil
}

// add adds a volume to the watch list
func (w *Watcher) add(v *structs.CSIVolume) error {
	w.wlock.Lock()
	defer w.wlock.Unlock()
	_, err := w.addLocked(v)
	return err
}

// addLocked adds a volume to the watch list and should only be called when
// locked. Creating the volumeWatcher starts a go routine to .watch() it
func (w *Watcher) addLocked(v *structs.CSIVolume) (*volumeWatcher, error) {
	// Not enabled so no-op
	if !w.enabled {
		return nil, nil
	}

	key := structs.NamespacedID{ID: v.ID, Namespace: v.Namespace}

	// Already watched so trigger an update for the volume
	if watcher, ok := w.watchers[key]; ok {
		watcher.Notify(v)
		return nil, nil
	}

	watcher := newVolumeWatcher(w, v)
	w.watchers[key] = watcher

	// Sending the first volume update here before we return ensures we've hit
	// the run loop in the goroutine before freeing the lock. This prevents a
	// race between shutting down the watcher and the blocking query.
	//
	// It also ensures that we don't drop events that happened during leadership
	// transitions and didn't get completed by the prior leader
	watcher.updateCh <- v
	return watcher, nil
}

// remove is called by a volume watcher's goroutine that wants to exit. It
// removes the watcher from the watch list and reports whether the goroutine
// may exit, which it may not if an update was queued for it.
//
// white room bugfix: remove deleted whatever watcher was stored under the
// key, so a goroutine of the previous leadership term that returned late
// from an RPC removed the new term's watcher for the same volume. It also
// ran after the goroutine had stopped reading updateCh, so an update
// queued in between was lost or started a second goroutine. Deciding under
// wlock, which Notify is always called with, closes both gaps.
func (w *Watcher) remove(key structs.NamespacedID, vw *volumeWatcher) bool {
	w.wlock.Lock()
	defer w.wlock.Unlock()
	if w.watchers[key] != vw {
		// flushed or replaced, so nothing can notify this watcher anymore
		return true
	}
	if len(vw.updateCh) > 0 {
		return false
	}
	delete(w.watchers, key)
	return true
}

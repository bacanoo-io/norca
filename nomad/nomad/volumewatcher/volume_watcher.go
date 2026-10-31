// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package volumewatcher

import (
	"context"
	"time"

	log "github.com/hashicorp/go-hclog"
	memdb "github.com/hashicorp/go-memdb"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// volumeWatcher is used to watch a single volume and trigger the
// scheduler when allocation health transitions.
type volumeWatcher struct {
	// v is the volume being watched
	v *structs.CSIVolume

	// state is the state that is watched for state changes.
	state *state.StateStore

	// server interface for CSI client RPCs
	rpc CSIVolumeRPC

	// the ACL needed to send RPCs
	leaderAcl string

	logger log.Logger

	// white room bugfix: the own ctx and exitFn fields were never set
	// and have been removed; the watcher is cancelled through shutdownCtx
	shutdownCtx context.Context // parent context

	// white room bugfix: deleteFn removed the map entry unconditionally
	// after the watch goroutine had already decided to exit, so it could
	// remove a newer watcher (another leadership term) and raced with
	// Notify. removeFn is called before exiting and reports whether the
	// goroutine may exit (see Watcher.remove)
	removeFn func() bool

	// quiescentTimeout is the time we wait until the volume has "settled"
	// before stopping the child watcher goroutines
	quiescentTimeout time.Duration

	// updateCh is triggered when there is an updated volume
	updateCh chan *structs.CSIVolume
}

// newVolumeWatcher returns a volume watcher that is used to watch
// volumes
func newVolumeWatcher(parent *Watcher, vol *structs.CSIVolume) *volumeWatcher {

	w := &volumeWatcher{
		updateCh:         make(chan *structs.CSIVolume, 1),
		v:                vol,
		state:            parent.state,
		rpc:              parent.rpc,
		leaderAcl:        parent.leaderAcl,
		logger:           parent.logger.With("volume_id", vol.ID, "namespace", vol.Namespace),
		shutdownCtx:      parent.ctx,
		quiescentTimeout: parent.quiescentTimeout,
	}
	key := structs.NamespacedID{ID: vol.ID, Namespace: vol.Namespace}
	w.removeFn = func() bool { return parent.remove(key, w) }

	// Start the long lived watcher that scans for allocation updates.
	//
	// white room bugfix: the running flag and the restart in Notify are
	// gone. A watcher found in the parent's watch list now always has a
	// live goroutine, because the goroutine leaves the list (under the
	// parent's lock) before it exits. Before, Notify could start a second
	// goroutine for a watcher whose first goroutine was about to remove
	// it from the list, leaving an untracked goroutine behind
	w.logger.Trace("starting watcher")
	go w.watch()
	return w
}

// Notify signals an update to the tracked volume. It is called with the
// parent's lock held, so it must not block.
func (vw *volumeWatcher) Notify(v *structs.CSIVolume) {
	// white room bugfix: this was a blocking send. While the watch
	// goroutine was in a slow Unpublish RPC with an update already queued,
	// it blocked with the parent's lock held, stalling every other volume
	// and the leader step-down (SetEnabled needs the lock to cancel
	// shutdownCtx). If the goroutine was instead exiting it waited for
	// the same lock in deleteFn, a deadlock. updateCh only wakes the
	// goroutine (it re-reads the volume from the state store), so an
	// update that is already queued covers this one.
	select {
	case vw.updateCh <- v:
	default:
	}
}

// watch is the long-running function that watches for changes to a volume.
// Each pass steps the volume's claims through the various states of reaping
// until the volume has no more claims eligible to be reaped.
func (vw *volumeWatcher) watch() {
	timer, stop := helper.NewSafeTimer(vw.quiescentTimeout)
	defer stop()

	for {
		select {
		// TODO(tgross): currently server->client RPC have no cancellation
		// context, so we can't stop the long-runner RPCs gracefully
		case <-vw.shutdownCtx.Done():
			// the parent has already flushed its watch list
			return
		case vol := <-vw.updateCh:
			vol = vw.getVolume(vol)
			if vol == nil {
				if vw.removeFn() {
					return
				}
				continue
			}
			vw.volumeReap(vol)
			timer.Reset(vw.quiescentTimeout)
		case <-timer.C:
			// Wait until the volume has "settled" before stopping this
			// goroutine so that we can handle the burst of updates around
			// freeing claims without having to spin it back up
			if vw.removeFn() {
				vw.logger.Trace("no more claims")
				return
			}
		}
	}
}

// getVolume returns the tracked volume, fully populated with the current
// state
func (vw *volumeWatcher) getVolume(vol *structs.CSIVolume) *structs.CSIVolume {
	var err error
	ws := memdb.NewWatchSet()

	vol, err = vw.state.CSIVolumeByID(ws, vol.Namespace, vol.ID)
	if err != nil {
		vw.logger.Error("could not query for volume", "error", err)
		return nil
	}
	if vol == nil {
		return nil
	}

	vol, err = vw.state.CSIVolumeDenormalize(ws, vol)
	if err != nil {
		vw.logger.Error("could not query allocs for volume", "error", err)
		return nil
	}
	vw.v = vol
	return vol
}

// volumeReap collects errors for logging but doesn't return them
// to the main loop.
func (vw *volumeWatcher) volumeReap(vol *structs.CSIVolume) {
	vw.logger.Trace("releasing unused volume claims")
	err := vw.volumeReapImpl(vol)
	if err != nil {
		vw.logger.Error("error releasing volume claims", "error", err)
	}
}

// volumeReapImpl unpublished all the volume's PastClaims. PastClaims
// will be populated from nil or terminal allocs when we call
// CSIVolumeDenormalize(), so this assumes we've done so in the caller
func (vw *volumeWatcher) volumeReapImpl(vol *structs.CSIVolume) error {
	var result *multierror.Error
	for _, claim := range vol.PastClaims {
		err := vw.unpublish(vol, claim)
		if err != nil {
			result = multierror.Append(result, err)
		}
	}
	return result.ErrorOrNil()
}

// white room bugfix: the unused isUnclaimed and collectPastClaims were
// removed; PastClaims are collected by the state store's
// CSIVolumeDenormalize, which getVolume calls.

func (vw *volumeWatcher) unpublish(vol *structs.CSIVolume, claim *structs.CSIVolumeClaim) error {
	vw.logger.Trace("unpublishing volume", "alloc", claim.AllocationID)
	req := &structs.CSIVolumeUnpublishRequest{
		VolumeID: vol.ID,
		Claim:    claim,
		WriteRequest: structs.WriteRequest{
			Namespace: vol.Namespace,
			Region:    vw.state.Config().Region,
			AuthToken: vw.leaderAcl,
		},
	}
	err := vw.rpc.Unpublish(req, &structs.CSIVolumeUnpublishResponse{})
	if err != nil {
		return err
	}
	claim.State = structs.CSIVolumeClaimStateReadyToFree
	return nil
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/hashicorp/raft"
	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/stream"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// After a restore, the first log that skips indexes above the restored state
// raises the base index of the event broker to the index before it (review
// finding MUK-16): a watcher that saw an index of the old history in between
// is refused and lists again, also once the state index is past it. A log
// that follows the restored state leaves the base as it is.
func TestFSM_RestoreHoleRaisesEventBase(t *testing.T) {
	ci.Parallel(t)

	exact := func(fsm *nomadFSM, index uint64) error {
		broker, err := fsm.State().EventBroker()
		must.NoError(t, err)
		sub, err := broker.Subscribe(&stream.SubscribeRequest{
			Topics:              map[structs.Topic][]string{structs.TopicAll: {"*"}},
			Namespace:           "*",
			Index:               index,
			StartExactlyAtIndex: true,
		})
		if sub != nil {
			sub.Unsubscribe()
		}
		return err
	}
	register := func(fsm *nomadFSM, index uint64) {
		req := structs.NodeRegisterRequest{Node: mock.Node()}
		buf, err := structs.Encode(structs.NodeRegisterRequestType, req)
		must.NoError(t, err)
		resp := fsm.Apply(&raft.Log{Index: index, Term: 1, Type: raft.LogCommand, Data: buf})
		if err, ok := resp.(error); ok {
			must.NoError(t, err)
		}
	}

	t.Run("hole", func(t *testing.T) {
		fsm := testFSM(t)
		register(fsm, 1000)
		restored := testSnapshotRestore(t, fsm)
		latest, err := restored.State().LatestIndex()
		must.NoError(t, err)
		must.Eq(t, uint64(1000), latest)

		// raft.Restore continues the log after the index the cluster had
		// reached (2000 here), above the indexes a watcher saw (1500).
		register(restored, 2001)
		must.ErrorIs(t, exact(restored, 1500), stream.ErrIndexNotAvailable)
		must.ErrorIs(t, exact(restored, 2000), stream.ErrIndexNotAvailable)
		must.NoError(t, exact(restored, 2001))
	})

	t.Run("no hole", func(t *testing.T) {
		fsm := testFSM(t)
		register(fsm, 1000)
		restored := testSnapshotRestore(t, fsm)
		register(restored, 1001)
		must.NoError(t, exact(restored, 1001))
		// only the first log after the restore is checked
		register(restored, 1500)
		must.NoError(t, exact(restored, 1100))
	})
}

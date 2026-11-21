// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package raftutil

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/bacanoo-io/norca/norca/helper/raftbadger"
	"github.com/hashicorp/raft"
	"github.com/shoenig/test/must"
)

// snapshotDir copies a flat directory: the files of a server that stopped at
// that moment without closing its store.
func snapshotDir(t *testing.T, src, dst string) {
	t.Helper()
	must.NoError(t, os.MkdirAll(dst, 0o755))
	ents, err := os.ReadDir(src)
	must.NoError(t, err)
	for _, e := range ents {
		if e.Name() == "LOCK" {
			continue
		}
		in, err := os.Open(filepath.Join(src, e.Name()))
		must.NoError(t, err)
		out, err := os.Create(filepath.Join(dst, e.Name()))
		must.NoError(t, err)
		_, err = io.Copy(out, in)
		must.NoError(t, err)
		must.NoError(t, in.Close())
		must.NoError(t, out.Close())
	}
}

// dirState is the names, sizes and times of the files of a directory.
func dirState(t *testing.T, dir string) map[string][2]int64 {
	t.Helper()
	out := map[string][2]int64{}
	ents, err := os.ReadDir(dir)
	must.NoError(t, err)
	for _, e := range ents {
		info, err := e.Info()
		must.NoError(t, err)
		out[e.Name()] = [2]int64{info.Size(), info.ModTime().UnixNano()}
	}
	return out
}

// The raft store of a server that did not stop cleanly is read from a copy:
// the commands work, and the store itself is not changed (review finding
// HLP-7: the read-only open failed with "Log truncate required").
//
// Not parallel: it counts the copies in the temporary directory.
func TestRaftStateInfo_StoreOfACrashedServer(t *testing.T) {

	base := t.TempDir()
	live := filepath.Join(base, "live")
	crashed := filepath.Join(base, "crashed")

	s, err := raftbadger.Open(raftbadger.Options{Path: live})
	must.NoError(t, err)
	logs := make([]*raft.Log, 0, 10)
	for i := 1; i <= 10; i++ {
		logs = append(logs, &raft.Log{Index: uint64(i), Term: 1, Type: raft.LogCommand, Data: []byte{1}})
	}
	must.NoError(t, s.StoreLogs(logs))
	snapshotDir(t, live, crashed)
	must.NoError(t, s.Close())

	before := dirState(t, crashed)
	tmp := os.TempDir()
	copiesBefore, _ := filepath.Glob(filepath.Join(tmp, "norca-raft-copy-*"))

	rs, first, last, err := RaftStateInfo(crashed)
	must.NoError(t, err)
	must.Eq(t, uint64(1), first)
	must.Eq(t, uint64(10), last)
	must.NoError(t, rs.Close())

	must.Eq(t, before, dirState(t, crashed), must.Sprint("the store was changed"))
	copiesAfter, _ := filepath.Glob(filepath.Join(tmp, "norca-raft-copy-*"))
	must.Eq(t, len(copiesBefore), len(copiesAfter), must.Sprint("the copy was not removed"))
}

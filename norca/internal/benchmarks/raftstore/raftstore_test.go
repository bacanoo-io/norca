// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package raftstore benchmarks the raft log and stable store of the servers
// (B1 of the storage and RPC plan). The same functions run against every
// store implementation, so the results of the stores can be compared:
//
//	go test -run '^$' -bench . -benchmem -count 10 ./internal/benchmarks/raftstore/
package raftstore

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	raftbench "github.com/hashicorp/raft/bench"
)

// store is a raft log store that is also a stable store, as the servers use
// it.
type store interface {
	raft.LogStore
	raft.StableStore
}

// newStore opens a store in a fresh directory; durable writes are on, as on
// a server.
type newStore func(b *testing.B) store

// stores lists the implementations to compare.
var stores = map[string]newStore{
	"bolt": func(b *testing.B) store {
		s, err := raftboltdb.New(raftboltdb.Options{Path: filepath.Join(b.TempDir(), "raft.db")})
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = s.Close() })
		return s
	},
}

func run(b *testing.B, fn func(*testing.B, store)) {
	for name, open := range stores {
		b.Run(name, func(b *testing.B) { fn(b, open(b)) })
	}
}

func BenchmarkFirstIndex(b *testing.B) {
	run(b, func(b *testing.B, s store) { raftbench.FirstIndex(b, s) })
}

func BenchmarkLastIndex(b *testing.B) {
	run(b, func(b *testing.B, s store) { raftbench.LastIndex(b, s) })
}

func BenchmarkGetLog(b *testing.B) {
	run(b, func(b *testing.B, s store) { raftbench.GetLog(b, s) })
}

func BenchmarkStoreLog(b *testing.B) {
	run(b, func(b *testing.B, s store) { raftbench.StoreLog(b, s) })
}

func BenchmarkStoreLogs(b *testing.B) {
	run(b, func(b *testing.B, s store) { raftbench.StoreLogs(b, s) })
}

func BenchmarkDeleteRange(b *testing.B) {
	run(b, func(b *testing.B, s store) { raftbench.DeleteRange(b, s) })
}

func BenchmarkSet(b *testing.B) {
	run(b, func(b *testing.B, s store) { raftbench.Set(b, s) })
}

func BenchmarkGet(b *testing.B) {
	run(b, func(b *testing.B, s store) { raftbench.Get(b, s) })
}

func BenchmarkSetUint64(b *testing.B) {
	run(b, func(b *testing.B, s store) { raftbench.SetUint64(b, s) })
}

func BenchmarkGetUint64(b *testing.B) {
	run(b, func(b *testing.B, s store) { raftbench.GetUint64(b, s) })
}

// BenchmarkAppend appends batches of entries of a given size, as the leader
// does under load: one call (one sync) per batch. The reported MB/s is the
// payload written.
func BenchmarkAppend(b *testing.B) {
	run(b, func(b *testing.B, s store) {
		for _, batch := range []int{1, 64, 512} {
			for _, size := range []int{1 << 10, 16 << 10} {
				name := fmt.Sprintf("batch=%d/size=%dKB", batch, size>>10)
				b.Run(name, func(b *testing.B) { appendBatches(b, s, batch, size) })
			}
		}
	})
}

func appendBatches(b *testing.B, s store, batch, size int) {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i)
	}
	logs := make([]*raft.Log, batch)
	for i := range logs {
		logs[i] = &raft.Log{Term: 1, Type: raft.LogCommand, Data: data}
	}
	b.SetBytes(int64(batch * size))
	last, err := s.LastIndex()
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	next := last + 1
	for i := 0; i < b.N; i++ {
		for _, l := range logs {
			l.Index = next
			next++
		}
		if err := s.StoreLogs(logs); err != nil {
			b.Fatal(err)
		}
	}
}

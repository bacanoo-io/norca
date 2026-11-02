// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package clientstate benchmarks the persistent state of a client (B2 of the
// storage and RPC plan): storing and reading allocations and task runner
// states, as the client does when allocations change and when it restarts.
//
//	go test -run '^$' -bench . -benchmem -count 10 ./internal/benchmarks/clientstate/
package clientstate

import (
	"fmt"
	"testing"

	trstate "github.com/bacanoo-io/norca/norca/client/allocrunner/taskrunner/state"
	"github.com/bacanoo-io/norca/norca/client/state"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// stores lists the implementations to compare.
var stores = map[string]func(b *testing.B) state.StateDB{
	"bolt": func(b *testing.B) state.StateDB {
		db, err := state.NewBoltStateDB(testlog.HCLogger(b), b.TempDir())
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = db.Close() })
		return db
	},
}

func run(b *testing.B, fn func(*testing.B, state.StateDB)) {
	for name, open := range stores {
		b.Run(name, func(b *testing.B) { fn(b, open(b)) })
	}
}

func allocs(n int) []*structs.Allocation {
	out := make([]*structs.Allocation, n)
	for i := range out {
		out[i] = mock.Alloc()
	}
	return out
}

// BenchmarkPutAllocation stores a changed allocation, as the client does on
// every allocation update.
func BenchmarkPutAllocation(b *testing.B) {
	run(b, func(b *testing.B, db state.StateDB) {
		a := mock.Alloc()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			a.AllocModifyIndex = uint64(i + 1) // a changed value, not deduplicated
			if err := db.PutAllocation(a); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkPutTaskRunnerLocalState stores the local state of a task runner,
// as the client does after each hook and driver step.
func BenchmarkPutTaskRunnerLocalState(b *testing.B) {
	run(b, func(b *testing.B, db state.StateDB) {
		a := mock.Alloc()
		if err := db.PutAllocation(a); err != nil {
			b.Fatal(err)
		}
		ls := trstate.NewLocalState()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ls.Hooks["bench"] = &trstate.HookState{Data: map[string]string{"n": fmt.Sprint(i)}}
			if err := db.PutTaskRunnerLocalState(a.ID, "web", ls); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkGetAllAllocations reads every allocation, as the client does when
// it restarts.
func BenchmarkGetAllAllocations(b *testing.B) {
	for _, n := range []int{100, 1000} {
		b.Run(fmt.Sprintf("allocs=%d", n), func(b *testing.B) {
			run(b, func(b *testing.B, db state.StateDB) {
				for _, a := range allocs(n) {
					if err := db.PutAllocation(a); err != nil {
						b.Fatal(err)
					}
				}
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					got, _, err := db.GetAllAllocations()
					if err != nil || len(got) != n {
						b.Fatalf("got %d allocations, error %v", len(got), err)
					}
				}
			})
		})
	}
}

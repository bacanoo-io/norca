package storage

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestMemoryBackend(t *testing.T) {
	runBackendConformance(t, func(_ *testing.T) Backend {
		return NewMemoryBackend()
	})
}

// TestMemoryBackendConcurrent is a race-detector smoke test: concurrent readers
// and writers on distinct keys must not trip the race detector or corrupt state.
func TestMemoryBackendConcurrent(t *testing.T) {
	b := NewMemoryBackend()
	ctx := context.Background()

	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		go func(n int) {
			defer wg.Done()
			key := fmt.Sprintf("worker/%d", n)
			for j := range 100 {
				if err := b.Put(ctx, &Entry{Key: key, Value: []byte{byte(j)}}); err != nil {
					t.Errorf("Put: %v", err)
					return
				}
				if _, err := b.Get(ctx, key); err != nil {
					t.Errorf("Get: %v", err)
					return
				}
				if _, err := b.List(ctx, "worker/"); err != nil {
					t.Errorf("List: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

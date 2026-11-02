// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tstorage

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shoenig/test/must"
)

// openTestStorage opens an on-disk storage with 10 second partitions.
func openTestStorage(t *testing.T, dir string) Storage {
	t.Helper()
	s, err := NewStorage(
		WithDataPath(dir),
		WithTimestampPrecision(Seconds),
		WithPartitionDuration(10*time.Second),
		WithRetention(time.Hour),
	)
	must.NoError(t, err)
	return s
}

func diskPartitionDirs(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "p-*"))
	must.NoError(t, err)
	return matches
}

// TestStorage_RemoveOldestDiskPartition checks that a disk partition is
// unmapped and removed (Windows removes no file with a mapped view), and
// that the stats follow.
func TestStorage_RemoveOldestDiskPartition(t *testing.T) {
	dir := t.TempDir()
	s := openTestStorage(t, dir)
	labels := []Label{{Name: "group", Value: "web"}}
	for ts := int64(1); ts <= 60; ts++ {
		must.NoError(t, s.InsertRows([]Row{{Metric: "cpu", Labels: labels, DataPoint: DataPoint{Timestamp: ts, Value: float64(ts)}}}))
	}
	must.NoError(t, s.Close())

	s = openTestStorage(t, dir)
	defer func() { must.NoError(t, s.Close()) }()
	st := s.Stats()
	must.Greater(t, 1, st.DiskPartitions)
	must.Positive(t, st.DiskBytes)
	must.Eq(t, 1, st.MinTimestamp)
	must.Eq(t, 60, st.MaxTimestamp)
	before := len(diskPartitionDirs(t, dir))
	must.Eq(t, st.DiskPartitions, before)

	points, err := s.Select("cpu", labels, 0, 100)
	must.NoError(t, err)
	must.Len(t, 60, points)

	removed, err := s.RemoveOldestDiskPartition()
	must.NoError(t, err)
	must.True(t, removed)
	must.Len(t, before-1, diskPartitionDirs(t, dir))
	after := s.Stats()
	must.Eq(t, st.DiskPartitions-1, after.DiskPartitions)
	must.Less(t, st.DiskBytes, after.DiskBytes)
	must.Greater(t, 1, after.MinTimestamp)

	points, err = s.Select("cpu", labels, 0, 100)
	must.NoError(t, err)
	must.Less(t, 60, len(points))
	must.Eq(t, 60, points[len(points)-1].Value)

	for {
		removed, err := s.RemoveOldestDiskPartition()
		must.NoError(t, err)
		if !removed {
			break
		}
	}
	must.SliceEmpty(t, diskPartitionDirs(t, dir))
	_, err = s.Select("cpu", labels, 0, 100)
	must.True(t, errors.Is(err, ErrNoDataPoints))
}

// TestStorage_SelectWhileRemoving checks that selects running while disk
// partitions are removed never read an unmapped partition.
func TestStorage_SelectWhileRemoving(t *testing.T) {
	dir := t.TempDir()
	s := openTestStorage(t, dir)
	labels := []Label{{Name: "group", Value: "web"}}
	for ts := int64(1); ts <= 200; ts++ {
		must.NoError(t, s.InsertRows([]Row{{Metric: "cpu", Labels: labels, DataPoint: DataPoint{Timestamp: ts, Value: 1}}}))
	}
	must.NoError(t, s.Close())
	s = openTestStorage(t, dir)
	defer func() { must.NoError(t, s.Close()) }()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, err := s.Select("cpu", labels, 0, 1000)
				if err != nil && !errors.Is(err, ErrNoDataPoints) {
					t.Errorf("select: %v", err)
					return
				}
			}
		}()
	}
	for {
		removed, err := s.RemoveOldestDiskPartition()
		must.NoError(t, err)
		if !removed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(stop)
	wg.Wait()
	_, err := os.Stat(dir)
	must.NoError(t, err)
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package metricsstore

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/internal/tstorage"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// seriesFile keeps the task groups with series, which tstorage can't
	// list.
	seriesFile = "series.json"

	// maintenanceInterval is how often the disk cap is enforced and the
	// series are saved.
	maintenanceInterval = time.Minute
)

// GroupKey identifies a task group.
type GroupKey struct {
	Namespace string
	Job       string
	Group     string
}

func (k GroupKey) labels() []tstorage.Label {
	return []tstorage.Label{{Name: "ns", Value: k.Namespace}, {Name: "job", Value: k.Job}, {Name: "group", Value: k.Group}}
}

// Store is the metrics store of a server.
type Store struct {
	logger hclog.Logger
	cfg    *Config
	db     tstorage.Storage

	mu sync.Mutex
	// pending are the windows not written yet, by their start (Unix
	// seconds).
	pending map[int64]map[GroupKey]*window
	// series are the task groups with series.
	series      map[GroupKey]struct{}
	seriesDirty bool
	// written is the end of the windows written so far: a later report of
	// such a window is dropped rather than written twice.
	written int64
	// newest is the time of the newest report.
	newest int64

	droppedReports atomic.Uint64
	droppedGroups  atomic.Uint64

	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

// Open opens the store of cfg, which must be enabled, and starts writing.
func Open(cfg *Config, logger hclog.Logger) (*Store, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, errors.New("the metrics store is disabled")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	opts := []tstorage.Option{
		tstorage.WithTimestampPrecision(tstorage.Seconds),
		tstorage.WithPartitionDuration(cfg.PartitionDuration),
		tstorage.WithRetention(cfg.Retention),
		tstorage.WithWALBufferedSize(cfg.WALBufferedSize),
	}
	if cfg.Dir != "" {
		opts = append(opts, tstorage.WithDataPath(cfg.Dir))
	}
	db, err := tstorage.NewStorage(opts...)
	if err != nil {
		return nil, fmt.Errorf("opening the metrics store: %w", err)
	}
	s := &Store{
		logger:  logger.Named("metrics_store"),
		cfg:     cfg.Copy(),
		db:      db,
		pending: map[int64]map[GroupKey]*window{},
		series:  map[GroupKey]struct{}{},
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
	if err := s.loadSeries(); err != nil {
		s.logger.Warn("the list of the task groups with series is lost; they appear again with their next report", "error", err)
	}
	go s.run()
	return s, nil
}

// Ingest adds a usage report received at (the time it was published) to
// the window of its sample interval. Reports more than a partition older
// than the newest one are dropped: tstorage takes points only into its two
// newest partitions.
func (s *Store) Ingest(report *structs.TaskGroupUsageReport, at time.Time) {
	if report == nil {
		return
	}
	interval := int64(s.cfg.SampleInterval / time.Second)
	ts := at.Unix()
	start := ts - ts%interval

	s.mu.Lock()
	defer s.mu.Unlock()
	if start+interval <= s.written || ts < s.newest-int64(s.cfg.PartitionDuration/time.Second) {
		s.droppedReports.Add(1)
		return
	}
	s.newest = max(s.newest, ts)
	groups := s.pending[start]
	if groups == nil {
		groups = map[GroupKey]*window{}
		s.pending[start] = groups
	}
	for _, g := range report.Groups {
		if g == nil || g.Samples <= 0 || g.JobID == "" || g.TaskGroup == "" {
			continue
		}
		ns := g.Namespace
		if ns == "" {
			ns = structs.DefaultNamespace
		}
		key := GroupKey{Namespace: ns, Job: g.JobID, Group: g.TaskGroup}
		if _, ok := s.series[key]; !ok {
			if s.cfg.MaxSeries > 0 && (len(s.series)+1)*seriesPerGroup > s.cfg.MaxSeries {
				s.droppedGroups.Add(1)
				continue
			}
			s.series[key] = struct{}{}
			s.seriesDirty = true
		}
		w := groups[key]
		if w == nil {
			w = &window{}
			groups[key] = w
		}
		w.addUsage(g)
	}
}

// AllocCounts are the running and the queued allocations of a task group.
type AllocCounts struct {
	Running int
	Queued  int
}

// RecordAllocs stores the allocations of the task groups sampled by the
// server at the time at. Task groups without series that have neither
// running nor queued allocations are left out.
func (s *Store) RecordAllocs(at time.Time, groups map[GroupKey]AllocCounts) {
	interval := int64(s.cfg.SampleInterval / time.Second)
	ts := at.Unix()
	start := ts - ts%interval
	running, queued := metricIndex(MetricAllocsRunning), metricIndex(MetricAllocsQueued)

	s.mu.Lock()
	defer s.mu.Unlock()
	if start+interval <= s.written {
		return
	}
	s.newest = max(s.newest, ts)
	windows := s.pending[start]
	if windows == nil {
		windows = map[GroupKey]*window{}
		s.pending[start] = windows
	}
	for key, n := range groups {
		if _, ok := s.series[key]; !ok {
			if n.Running == 0 && n.Queued == 0 {
				continue
			}
			if s.cfg.MaxSeries > 0 && (len(s.series)+1)*seriesPerGroup > s.cfg.MaxSeries {
				s.droppedGroups.Add(1)
				continue
			}
			s.series[key] = struct{}{}
			s.seriesDirty = true
		}
		w := windows[key]
		if w == nil {
			w = &window{}
			windows[key] = w
		}
		w.setState(running, float64(n.Running))
		w.setState(queued, float64(n.Queued))
	}
}

// run writes the finished windows and enforces the disk cap until Close.
func (s *Store) run() {
	defer close(s.doneCh)
	tick := time.NewTicker(max(s.cfg.SampleInterval/3, time.Second))
	defer tick.Stop()
	maintain := time.NewTicker(maintenanceInterval)
	defer maintain.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case now := <-tick.C:
			// A window is written one interval after it ended, so that late
			// reports still count.
			if err := s.flush(now.Add(-s.cfg.SampleInterval)); err != nil {
				s.logger.Warn("writing the usage failed", "error", err)
			}
		case <-maintain.C:
			s.maintain()
		}
	}
}

// flush writes the windows that ended before until.
func (s *Store) flush(until time.Time) error {
	interval := int64(s.cfg.SampleInterval / time.Second)
	limit := until.Unix()

	s.mu.Lock()
	var rows []tstorage.Row
	for start, groups := range s.pending {
		if start+interval > limit {
			continue
		}
		for key, w := range groups {
			labels := key.labels()
			for i, d := range groupMetricDefs {
				avg, mx, ok := w.value(i)
				if !ok {
					continue
				}
				rows = append(rows,
					tstorage.Row{Metric: d.name + ".avg", Labels: labels,
						DataPoint: tstorage.DataPoint{Timestamp: start, Value: avg}},
					tstorage.Row{Metric: d.name + ".max", Labels: labels,
						DataPoint: tstorage.DataPoint{Timestamp: start, Value: mx}})
			}
		}
		delete(s.pending, start)
	}
	if aligned := limit - limit%interval; aligned > s.written {
		s.written = aligned
	}
	s.mu.Unlock()

	if len(rows) == 0 {
		return nil
	}
	// tstorage finds the points of a series in memory only in time order.
	slices.SortStableFunc(rows, func(a, b tstorage.Row) int { return cmp.Compare(a.Timestamp, b.Timestamp) })
	return s.db.InsertRows(rows)
}

// maintain removes the oldest partitions above the disk cap and saves the
// series.
func (s *Store) maintain() {
	if s.cfg.MaxDiskSize > 0 {
		for s.db.Stats().DiskBytes > s.cfg.MaxDiskSize {
			removed, err := s.db.RemoveOldestDiskPartition()
			if err != nil {
				s.logger.Warn("removing the oldest partition above max_disk_size failed", "error", err)
				break
			}
			if !removed {
				break
			}
			s.logger.Info("removed the oldest partition above max_disk_size", "max_disk_size", s.cfg.MaxDiskSize)
		}
	}
	if err := s.saveSeries(); err != nil {
		s.logger.Warn("saving the list of the task groups with series failed", "error", err)
	}
}

// Close writes the pending windows and closes the store.
func (s *Store) Close() error {
	var err error
	s.stopOnce.Do(func() {
		close(s.stopCh)
		<-s.doneCh
		err = errors.Join(
			s.flush(time.Unix(1<<62, 0)),
			s.saveSeries(),
			s.db.Close(),
		)
	})
	return err
}

// UsageWindow is the usage of a task group in one sample interval, as the
// store keeps it: one sample per interval.
type UsageWindow struct {
	Start time.Time
	Usage *structs.TaskGroupUsage
}

// usageMetrics are the values of a UsageWindow.
var usageMetrics = []struct {
	name  string
	value func(*structs.TaskGroupUsage) *structs.TaskGroupUsageValue
}{
	{MetricCPU, func(g *structs.TaskGroupUsage) *structs.TaskGroupUsageValue { return &g.CPUPercent }},
	{MetricCPUMHz, func(g *structs.TaskGroupUsage) *structs.TaskGroupUsageValue { return &g.CPUMHz }},
	{MetricMemory, func(g *structs.TaskGroupUsage) *structs.TaskGroupUsageValue { return &g.MemoryPercent }},
	{MetricMemoryMB, func(g *structs.TaskGroupUsage) *structs.TaskGroupUsageValue { return &g.MemoryMB }},
}

// UsageSince returns the CPU and memory usage of every task group since
// from, by sample interval: the stored points and the windows not written
// yet. It seeds the built-in autoscaler of a new leader.
func (s *Store) UsageSince(from time.Time) ([]UsageWindow, error) {
	type wkey struct {
		group GroupKey
		start int64
	}
	found := map[wkey]*structs.TaskGroupUsage{}
	get := func(k GroupKey, start int64) *structs.TaskGroupUsage {
		g := found[wkey{k, start}]
		if g == nil {
			g = &structs.TaskGroupUsage{Namespace: k.Namespace, JobID: k.Job, TaskGroup: k.Group, Samples: 1}
			found[wkey{k, start}] = g
		}
		return g
	}

	// The windows not written yet.
	s.mu.Lock()
	for start, groups := range s.pending {
		if start < from.Unix() {
			continue
		}
		for k, w := range groups {
			if !w.has[metricIndex(MetricCPU)] {
				continue // only allocations
			}
			g := get(k, start)
			for _, m := range usageMetrics {
				avg, mx, _ := w.value(metricIndex(m.name))
				*m.value(g) = structs.TaskGroupUsageValue{Avg: avg, Max: mx}
			}
		}
	}
	s.mu.Unlock()

	end := time.Now().Add(s.cfg.SampleInterval).Unix()
	for _, k := range s.Groups() {
		labels := k.labels()
		for _, m := range usageMetrics {
			avg, err := s.selectPoints(m.name+".avg", labels, from.Unix(), end)
			if err != nil {
				return nil, err
			}
			mx, err := s.selectPoints(m.name+".max", labels, from.Unix(), end)
			if err != nil {
				return nil, err
			}
			for _, p := range avg {
				m.value(get(k, p.Timestamp)).Avg = p.Value
			}
			for _, p := range mx {
				m.value(get(k, p.Timestamp)).Max = p.Value
			}
		}
	}

	out := make([]UsageWindow, 0, len(found))
	for k, g := range found {
		out = append(out, UsageWindow{Start: time.Unix(k.start, 0), Usage: g})
	}
	slices.SortFunc(out, func(a, b UsageWindow) int { return a.Start.Compare(b.Start) })
	return out, nil
}

// Groups returns the task groups with series, sorted.
func (s *Store) Groups() []GroupKey {
	s.mu.Lock()
	out := make([]GroupKey, 0, len(s.series))
	for k := range s.series {
		out = append(out, k)
	}
	s.mu.Unlock()
	slices.SortFunc(out, func(a, b GroupKey) int {
		return strings.Compare(a.Namespace+"\x00"+a.Job+"\x00"+a.Group, b.Namespace+"\x00"+b.Job+"\x00"+b.Group)
	})
	return out
}

func (s *Store) loadSeries() error {
	if s.cfg.Dir == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(s.cfg.Dir, seriesFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var keys []GroupKey
	if err := json.Unmarshal(b, &keys); err != nil {
		return err
	}
	for _, k := range keys {
		s.series[k] = struct{}{}
	}
	return nil
}

// saveSeries writes the series when they changed, atomically.
func (s *Store) saveSeries() error {
	if s.cfg.Dir == "" {
		return nil
	}
	s.mu.Lock()
	if !s.seriesDirty {
		s.mu.Unlock()
		return nil
	}
	s.seriesDirty = false
	s.mu.Unlock()

	b, err := json.Marshal(s.Groups())
	if err != nil {
		return err
	}
	path := filepath.Join(s.cfg.Dir, seriesFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		s.mu.Lock()
		s.seriesDirty = true
		s.mu.Unlock()
	}
	return err
}

// Status is the state of the store.
type Status struct {
	Partitions     int
	DiskPartitions int
	DiskBytes      int64
	MaxDiskSize    int64
	Series         int
	MaxSeries      int
	// Oldest and Newest are the times of the oldest and the newest point.
	Oldest time.Time
	Newest time.Time
	// DroppedReports counts the reports too old to store or late for a
	// window already written, DroppedGroups the
	// task groups left out at max_series.
	DroppedReports uint64
	DroppedGroups  uint64
	Retention      time.Duration
	SampleInterval time.Duration
}

// Status returns the state of the store.
func (s *Store) Status() *Status {
	st := s.db.Stats()
	s.mu.Lock()
	series := len(s.series) * seriesPerGroup
	s.mu.Unlock()
	out := &Status{
		Partitions:     st.Partitions,
		DiskPartitions: st.DiskPartitions,
		DiskBytes:      st.DiskBytes,
		MaxDiskSize:    s.cfg.MaxDiskSize,
		Series:         series,
		MaxSeries:      s.cfg.MaxSeries,
		DroppedReports: s.droppedReports.Load(),
		DroppedGroups:  s.droppedGroups.Load(),
		Retention:      s.cfg.Retention,
		SampleInterval: s.cfg.SampleInterval,
	}
	if st.MinTimestamp > 0 {
		out.Oldest = time.Unix(st.MinTimestamp, 0).UTC()
	}
	if st.MaxTimestamp > 0 {
		out.Newest = time.Unix(st.MaxTimestamp, 0).UTC()
	}
	return out
}

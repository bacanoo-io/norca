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

	"github.com/bacanoo-io/norca/norca/internal/tstorage"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/hashicorp/go-hclog"
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

// database is one of the two tstorage databases of the store, with its own
// retention, disk cap, WAL and partitions.
type database struct {
	db tstorage.Storage

	// retention and maxDiskSize are the effective limits, which a reload
	// changes (nanoseconds, bytes).
	retention   atomic.Int64
	maxDiskSize atomic.Int64

	// retentionName and diskName are the settings of the limits, for the
	// messages.
	retentionName string
	diskName      string
}

// openDatabase opens a database in dir (in memory when empty).
func openDatabase(cfg *Config, dir string, retention time.Duration, maxDiskSize int64, retentionName, diskName string) (*database, error) {
	opts := []tstorage.Option{
		tstorage.WithTimestampPrecision(tstorage.Seconds),
		tstorage.WithPartitionDuration(cfg.PartitionDuration),
		tstorage.WithRetention(retention),
		tstorage.WithWALBufferedSize(cfg.WALBufferedSize),
	}
	if dir != "" {
		opts = append(opts, tstorage.WithDataPath(dir))
	}
	db, err := tstorage.NewStorage(opts...)
	if err != nil {
		return nil, err
	}
	d := &database{db: db, retentionName: retentionName, diskName: diskName}
	d.retention.Store(int64(retention))
	d.maxDiskSize.Store(maxDiskSize)
	return d, nil
}

// insert writes rows sorted by time.
func (d *database) insert(rows []tstorage.Row) error {
	if len(rows) == 0 {
		return nil
	}
	return d.db.InsertRows(rows)
}

// status returns the state of the database; series is the number of its
// series.
func (d *database) status(series int) DatabaseStatus {
	st := d.db.Stats()
	out := DatabaseStatus{
		Partitions:     st.Partitions,
		DiskPartitions: st.DiskPartitions,
		DiskBytes:      st.DiskBytes,
		MaxDiskSize:    d.maxDiskSize.Load(),
		Series:         series,
		Retention:      time.Duration(d.retention.Load()),
	}
	if st.MinTimestamp > 0 {
		out.Oldest = time.Unix(st.MinTimestamp, 0).UTC()
	}
	if st.MaxTimestamp > 0 {
		out.Newest = time.Unix(st.MaxTimestamp, 0).UTC()
	}
	return out
}

// Store is the metrics store of a server. It keeps its points in two
// databases: the host metrics (see HostMetric) in the hosts store, with their
// own retention and disk cap, and every other metric in the main store. The
// index of the series, the limit max_series and the series file (in the
// directory of the main store) cover both.
type Store struct {
	logger hclog.Logger
	// cfg is the configuration the store was opened with; a reload changes
	// cfg.MaxSeries (under mu), and the retention and the disk cap of the
	// databases.
	cfg *Config

	main  *database
	hosts *database

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
	// streamAt and streamSeq are the publish time and the stream sequence
	// of the newest report taken from a stream, saved with the series: a
	// report at or before them is a redelivery.
	streamAt  int64
	streamSeq uint64

	// gauges are the gauge series by gaugeKey, gaugePending their values
	// not written yet by window start.
	gauges       map[string]GaugeSeries
	gaugePending map[int64]map[string]gaugeValue

	// groupLast and gaugeLast are the start of the newest window with a
	// point of each task group and gauge series (Unix seconds): a series
	// without a point within the retention is forgotten (see forget).
	groupLast map[GroupKey]int64
	gaugeLast map[string]int64

	// live is the latest usage report of each node.
	live map[string]*nodeLive

	forgottenSeries  atomic.Uint64
	droppedReports   atomic.Uint64
	droppedGroups    atomic.Uint64
	duplicateReports atomic.Uint64

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
	cfg = cfg.Copy()
	if cfg.HostDir == "" && cfg.Dir != "" {
		cfg.HostDir = cfg.Dir + "-hosts"
	}
	main, err := openDatabase(cfg, cfg.Dir, cfg.Retention, cfg.MaxDiskSize, "retention", "max_disk_size")
	if err != nil {
		return nil, fmt.Errorf("opening the metrics store: %w", err)
	}
	hosts, err := openDatabase(cfg, cfg.HostDir, cfg.HostRetention, cfg.HostMaxDiskSize, "host_retention", "host_max_disk_size")
	if err != nil {
		_ = main.db.Close()
		return nil, fmt.Errorf("opening the hosts store of the metrics store: %w", err)
	}
	s := &Store{
		logger:       logger.Named("metrics_store"),
		cfg:          cfg,
		main:         main,
		hosts:        hosts,
		pending:      map[int64]map[GroupKey]*window{},
		series:       map[GroupKey]struct{}{},
		gauges:       map[string]GaugeSeries{},
		gaugePending: map[int64]map[string]gaugeValue{},
		groupLast:    map[GroupKey]int64{},
		gaugeLast:    map[string]int64{},
		live:         map[string]*nodeLive{},
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ingestLocked(report, at)
}

// IngestStream is Ingest for a report read from a stream in order, with its
// stream sequence, and tells whether the report was new. The stream delivers
// a report again when its acknowledgement was lost, also after a restart, and
// a report must count once: the windows average the reports and the live
// usage adds them up. The publish time orders the reports across a stream
// that was created again, which starts its sequences over.
func (s *Store) IngestStream(report *structs.TaskGroupUsageReport, at time.Time, seq uint64) bool {
	if report == nil {
		return false
	}
	nanos := at.UnixNano()
	s.mu.Lock()
	defer s.mu.Unlock()
	if nanos < s.streamAt || (nanos == s.streamAt && seq == s.streamSeq) {
		s.duplicateReports.Add(1)
		return false
	}
	s.streamAt, s.streamSeq = nanos, seq
	s.seriesDirty = true
	s.ingestLocked(report, at)
	return true
}

// ingestLocked adds a report to its window; s.mu is held.
func (s *Store) ingestLocked(report *structs.TaskGroupUsageReport, at time.Time) {
	interval := int64(s.cfg.SampleInterval / time.Second)
	ts := at.Unix()
	start := ts - ts%interval

	s.recordLiveLocked(report, at)
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
			if s.cfg.MaxSeries > 0 && s.seriesCountLocked()+seriesPerGroup > s.cfg.MaxSeries {
				s.droppedGroups.Add(1)
				continue
			}
			s.series[key] = struct{}{}
			s.seriesDirty = true
		}
		s.groupLast[key] = max(s.groupLast[key], start)
		w := groups[key]
		if w == nil {
			w = &window{}
			groups[key] = w
		}
		w.addUsage(g)
	}
}

// AllocCounts are the allocations of a task group by status, and the task
// restarts of its allocations since the previous sample.
type AllocCounts struct {
	Running  int
	Queued   int
	Starting int
	Failed   int
	Lost     int
	Restarts int
}

// active reports whether the group has work: allocations running, queued or
// starting, or restarts. Failed and lost allocations alone do not make a
// series: the summaries of old jobs keep them.
func (c AllocCounts) active() bool {
	return c.Running > 0 || c.Queued > 0 || c.Starting > 0 || c.Restarts > 0
}

// RecordAllocs stores the allocations of the task groups sampled by the
// server at the time at. Task groups without series and without work (see
// AllocCounts.active) are left out.
func (s *Store) RecordAllocs(at time.Time, groups map[GroupKey]AllocCounts) {
	interval := int64(s.cfg.SampleInterval / time.Second)
	ts := at.Unix()
	start := ts - ts%interval
	running, queued := metricIndex(MetricAllocsRunning), metricIndex(MetricAllocsQueued)
	starting, failed, lost := metricIndex(MetricAllocsStarting), metricIndex(MetricAllocsFailed), metricIndex(MetricAllocsLost)
	restarts := metricIndex(MetricRestarts)

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
			if !n.active() {
				continue
			}
			if s.cfg.MaxSeries > 0 && s.seriesCountLocked()+seriesPerGroup > s.cfg.MaxSeries {
				s.droppedGroups.Add(1)
				continue
			}
			s.series[key] = struct{}{}
			s.seriesDirty = true
		}
		s.groupLast[key] = max(s.groupLast[key], start)
		w := windows[key]
		if w == nil {
			w = &window{}
			windows[key] = w
		}
		w.setState(running, float64(n.Running))
		w.setState(queued, float64(n.Queued))
		w.setState(starting, float64(n.Starting))
		w.setState(failed, float64(n.Failed))
		w.setState(lost, float64(n.Lost))
		w.addCount(restarts, float64(n.Restarts))
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
	rows = append(rows, s.gaugeRowsLocked(limit, interval)...)
	if aligned := limit - limit%interval; aligned > s.written {
		s.written = aligned
	}
	s.mu.Unlock()

	if len(rows) == 0 {
		return nil
	}
	// tstorage finds the points of a series in memory only in time order.
	slices.SortStableFunc(rows, func(a, b tstorage.Row) int { return cmp.Compare(a.Timestamp, b.Timestamp) })
	var mainRows, hostRows []tstorage.Row
	for _, r := range rows {
		if HostMetric(r.Metric) {
			hostRows = append(hostRows, r)
		} else {
			mainRows = append(mainRows, r)
		}
	}
	return errors.Join(s.main.insert(mainRows), s.hosts.insert(hostRows))
}

// db returns the database that keeps a metric (with or without the .avg or
// .max of its series).
func (s *Store) db(metric string) *database {
	if HostMetric(metric) {
		return s.hosts
	}
	return s.main
}

// maintain removes the oldest partitions above the disk cap of each
// database, forgets the series without a point within the retention and
// saves the series.
func (s *Store) maintain() {
	if n := s.forget(time.Now()); n > 0 {
		s.logger.Debug("forgot the series without a point within the retention", "series", n)
	}
	s.enforceDiskCap(s.main)
	s.enforceDiskCap(s.hosts)
	if err := s.saveSeries(); err != nil {
		s.logger.Warn("saving the list of the task groups with series failed", "error", err)
	}
}

// enforceDiskCap removes the oldest partitions of a database while it is
// above its disk cap.
func (s *Store) enforceDiskCap(d *database) {
	maxDisk := d.maxDiskSize.Load()
	if maxDisk <= 0 {
		return
	}
	for d.db.Stats().DiskBytes > maxDisk {
		removed, err := d.db.RemoveOldestDiskPartition()
		if err != nil {
			s.logger.Warn("removing the oldest partition above "+d.diskName+" failed", "error", err)
			return
		}
		if !removed {
			return
		}
		s.logger.Info("removed the oldest partition above "+d.diskName, d.diskName, maxDisk)
	}
}

// forget removes the task groups and the gauge series without a point within
// the retention of their database before now from the index of the series,
// and returns how many series that are (as counted toward max_series): a host
// series is forgotten after host_retention, any other after retention. They
// no longer count toward max_series nor appear in the list of series; a
// forgotten series that gets a point again starts again. Their points stay
// until their partition expires. A series with a point in a window not
// written yet is kept: the index and the windows change under s.mu only, so a
// point that comes meanwhile either keeps the series or starts it again.
func (s *Store) forget(now time.Time) int {
	cutoff := now.Add(-s.Retention()).Unix()
	hostCutoff := now.Add(-s.HostRetention()).Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for key := range s.series {
		last, ok := s.groupLast[key]
		if !ok {
			s.groupLast[key] = now.Unix() // not known: a full retention from now
			continue
		}
		if last >= cutoff || s.groupPendingLocked(key) {
			continue
		}
		delete(s.series, key)
		delete(s.groupLast, key)
		n += seriesPerGroup
	}
	for key, g := range s.gauges {
		last, ok := s.gaugeLast[key]
		if !ok {
			s.gaugeLast[key] = now.Unix()
			continue
		}
		limit := cutoff
		if HostMetric(g.Metric) {
			limit = hostCutoff
		}
		if last >= limit || s.gaugePendingLocked(key) {
			continue
		}
		delete(s.gauges, key)
		delete(s.gaugeLast, key)
		n += 2
	}
	if n > 0 {
		s.seriesDirty = true
		s.forgottenSeries.Add(uint64(n))
	}
	return n
}

// groupPendingLocked reports whether a task group has a window not written
// yet; s.mu is held.
func (s *Store) groupPendingLocked(key GroupKey) bool {
	for _, groups := range s.pending {
		if _, ok := groups[key]; ok {
			return true
		}
	}
	return false
}

// gaugePendingLocked reports whether a gauge series has a window not written
// yet; s.mu is held.
func (s *Store) gaugePendingLocked(key string) bool {
	for _, values := range s.gaugePending {
		if _, ok := values[key]; ok {
			return true
		}
	}
	return false
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
			s.main.db.Close(),
			s.hosts.db.Close(),
		)
	})
	return err
}

// SetRetention changes the retention of the main store at run time. A
// shorter retention removes the partitions now past it at once; a longer one
// keeps what is still on disk longer, and what was removed does not come
// back.
func (s *Store) SetRetention(retention time.Duration) error {
	return s.setRetention(s.main, retention)
}

// SetHostRetention changes the retention of the hosts store at run time,
// like SetRetention.
func (s *Store) SetHostRetention(retention time.Duration) error {
	return s.setRetention(s.hosts, retention)
}

func (s *Store) setRetention(d *database, retention time.Duration) error {
	if err := checkRetention(d.retentionName, retention); err != nil {
		return err
	}
	select {
	case <-s.stopCh:
		return errors.New("the metrics store is closed")
	default:
	}
	if err := d.db.SetRetention(retention); err != nil {
		return fmt.Errorf("setting the %s of the metrics store: %w", d.retentionName, err)
	}
	d.retention.Store(int64(retention))
	return nil
}

// Retention returns the retention of the main store in effect.
func (s *Store) Retention() time.Duration {
	return time.Duration(s.main.retention.Load())
}

// HostRetention returns the retention of the hosts store in effect.
func (s *Store) HostRetention() time.Duration {
	return time.Duration(s.hosts.retention.Load())
}

// RetentionOf returns the retention in effect of the database that keeps a
// metric: HostRetention for a host metric, Retention for any other.
func (s *Store) RetentionOf(metric string) time.Duration {
	return time.Duration(s.db(metric).retention.Load())
}

// Reload applies a reloaded configuration. The retention, host_retention,
// max_disk_size, host_max_disk_size and max_series apply at once: the disk
// caps at the next maintenance (within a minute), max_series to new series
// only. partition_duration,
// sample_interval and wal_buffered_size shape the files and the sampling and
// need a restart: a change of them is logged and ignored.
func (s *Store) Reload(cfg *Config) error {
	if cfg == nil {
		return nil
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	var restart []string
	if cfg.PartitionDuration != s.cfg.PartitionDuration {
		restart = append(restart, "partition_duration")
	}
	if cfg.SampleInterval != s.cfg.SampleInterval {
		restart = append(restart, "sample_interval")
	}
	if cfg.WALBufferedSize != s.cfg.WALBufferedSize {
		restart = append(restart, "wal_buffered_size")
	}
	if len(restart) > 0 {
		s.logger.Warn("changed metrics_store settings apply only after a restart", "settings", restart,
			"partition_duration", s.cfg.PartitionDuration, "sample_interval", s.cfg.SampleInterval)
	}

	for _, l := range []struct {
		d           *database
		retention   time.Duration
		maxDiskSize int64
	}{
		{s.main, cfg.Retention, cfg.MaxDiskSize},
		{s.hosts, cfg.HostRetention, cfg.HostMaxDiskSize},
	} {
		if old := time.Duration(l.d.retention.Load()); old != l.retention {
			if err := s.setRetention(l.d, l.retention); err != nil {
				return err
			}
			s.logger.Info("metrics store "+l.d.retentionName+" changed", "from", FormatRetention(old), "to", FormatRetention(l.retention))
		}
		if old := l.d.maxDiskSize.Swap(l.maxDiskSize); old != l.maxDiskSize {
			s.logger.Info("metrics store "+l.d.diskName+" changed", "from", old, "to", l.maxDiskSize)
		}
	}
	s.mu.Lock()
	old := s.cfg.MaxSeries
	s.cfg.MaxSeries = cfg.MaxSeries
	s.mu.Unlock()
	if old != cfg.MaxSeries {
		s.logger.Info("metrics store max_series changed", "from", old, "to", cfg.MaxSeries)
	}
	return nil
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
	var saved savedSeries
	if err := json.Unmarshal(b, &saved); err != nil {
		return err
	}
	s.streamAt, s.streamSeq, s.written = saved.StreamAt, saved.StreamSeq, saved.Written
	// A series without the time of its newest point (a file of an older
	// version) gets a full retention from now.
	now := time.Now().Unix()
	lastOr := func(id string) int64 {
		if last, ok := saved.Last[id]; ok {
			return last
		}
		return now
	}
	for _, k := range saved.Groups {
		s.series[k] = struct{}{}
		s.groupLast[k] = lastOr(groupLastID(k))
	}
	for _, g := range saved.Gauges {
		if _, ok := gaugeLabels[g.Metric]; ok {
			key := gaugeKey(g.Metric, g.Labels)
			s.gauges[key] = g
			s.gaugeLast[key] = lastOr(key)
		}
	}
	return nil
}

// groupLastID is the key of a task group in savedSeries.Last; the keys of
// the gauge series (gaugeKey) start with the metric name instead.
func groupLastID(k GroupKey) string {
	return "\x00" + k.Namespace + "\x00" + k.Job + "\x00" + k.Group
}

// lastLocked returns the times of the newest points by savedSeries.Last
// key; s.mu is held.
func (s *Store) lastLocked() map[string]int64 {
	out := make(map[string]int64, len(s.groupLast)+len(s.gaugeLast))
	for k, v := range s.groupLast {
		out[groupLastID(k)] = v
	}
	for k, v := range s.gaugeLast {
		out[k] = v
	}
	return out
}

// savedSeries is the content of the series file.
type savedSeries struct {
	Groups []GroupKey
	Gauges []GaugeSeries

	// StreamAt and StreamSeq are the position in the stream of the reports,
	// Written the end of the windows written: a window is written once, also
	// across a restart.
	StreamAt  int64
	StreamSeq uint64
	Written   int64

	// Last is the start of the newest window with a point of each series
	// (Unix seconds), by groupLastID for the task groups and gaugeKey for the
	// gauges.
	Last map[string]int64 `json:",omitempty"`
}

// seriesCountLocked is the number of series; s.mu is held.
func (s *Store) seriesCountLocked() int {
	return len(s.series)*seriesPerGroup + 2*len(s.gauges)
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
	streamAt, streamSeq := s.streamAt, s.streamSeq
	last := s.lastLocked()
	// Close flushes with a limit in the far future: what is written ends
	// with the window of the newest report.
	interval := int64(s.cfg.SampleInterval / time.Second)
	written := min(s.written, s.newest-s.newest%interval+interval)
	s.mu.Unlock()

	b, err := json.Marshal(savedSeries{Groups: s.Groups(), Gauges: s.Gauges(),
		StreamAt: streamAt, StreamSeq: streamSeq, Written: written, Last: last})
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

// DatabaseStatus is the state of one of the databases of the store.
type DatabaseStatus struct {
	Partitions     int
	DiskPartitions int
	DiskBytes      int64
	MaxDiskSize    int64
	// Series is the number of the series of the database.
	Series int
	// Oldest and Newest are the times of the oldest and the newest point.
	Oldest    time.Time
	Newest    time.Time
	Retention time.Duration
}

// Status is the state of the store: the fields of the main store, and the
// hosts store in Hosts.
type Status struct {
	Partitions     int
	DiskPartitions int
	DiskBytes      int64
	MaxDiskSize    int64
	// Series is the number of the series of both databases, which
	// MaxSeries limits.
	Series    int
	MaxSeries int
	// Oldest and Newest are the times of the oldest and the newest point.
	Oldest time.Time
	Newest time.Time
	// DroppedReports counts the reports too old to store or late for a
	// window already written, DroppedGroups the
	// task groups left out at max_series.
	DroppedReports uint64
	DroppedGroups  uint64
	// DuplicateReports counts the reports a stream delivered again.
	DuplicateReports uint64
	// ForgottenSeries counts the series removed from the index for having no
	// point within the retention (as counted toward max_series).
	ForgottenSeries uint64
	Retention       time.Duration
	SampleInterval  time.Duration

	// Hosts is the state of the hosts store.
	Hosts DatabaseStatus
}

// Status returns the state of the store.
func (s *Store) Status() *Status {
	s.mu.Lock()
	series := s.seriesCountLocked()
	hostSeries := 0
	for _, g := range s.gauges {
		if HostMetric(g.Metric) {
			hostSeries += 2
		}
	}
	maxSeries := s.cfg.MaxSeries
	s.mu.Unlock()
	main := s.main.status(series - hostSeries)
	return &Status{
		Partitions:       main.Partitions,
		DiskPartitions:   main.DiskPartitions,
		DiskBytes:        main.DiskBytes,
		MaxDiskSize:      main.MaxDiskSize,
		Series:           series,
		MaxSeries:        maxSeries,
		Oldest:           main.Oldest,
		Newest:           main.Newest,
		DroppedReports:   s.droppedReports.Load(),
		DroppedGroups:    s.droppedGroups.Load(),
		DuplicateReports: s.duplicateReports.Load(),
		ForgottenSeries:  s.forgottenSeries.Load(),
		Retention:        main.Retention,
		SampleInterval:   s.cfg.SampleInterval,
		Hosts:            s.hosts.status(hostSeries),
	}
}

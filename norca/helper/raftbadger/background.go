// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package raftbadger

import (
	"errors"
	"fmt"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/hashicorp/go-hclog"
	metrics "github.com/hashicorp/go-metrics"
)

// gcLoop runs the value log garbage collection once per interval: it
// rewrites value log files while more than half of a file is garbage, and
// stops at the first run that finds nothing to rewrite. With values up to
// 1 MB kept in the LSM tree it rarely has work.
func (s *Store) gcLoop() {
	defer s.done.Done()
	if s.opts.InMemory {
		return
	}
	t := time.NewTicker(s.opts.ValueLogGCInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
		start := time.Now()
		files := 0
		for {
			select {
			case <-s.stop:
				return
			default:
			}
			err := s.db.RunValueLogGC(0.5)
			if err != nil {
				if !errors.Is(err, badger.ErrNoRewrite) && !errors.Is(err, badger.ErrRejected) {
					s.logger.Warn("value log garbage collection failed", "error", err)
				}
				break
			}
			files++
		}
		s.measureSince("vlog_gc", start)
		if files > 0 {
			s.logger.Debug("value log garbage collection", "rewritten_files", files, "took", time.Since(start))
		}
	}
}

// metricsLoop reports the shape of the LSM tree every 10 seconds; level 0
// close to NumLevelZeroTablesStall means writes are about to stall.
func (s *Store) metricsLoop() {
	defer s.done.Done()
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	warned := false
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
		lsm, vlog := s.db.Size()
		s.gauge("lsm_bytes", float32(lsm))
		s.gauge("vlog_bytes", float32(vlog))
		l0 := 0
		for _, l := range s.db.Levels() {
			s.gauge(fmt.Sprintf("level%d_tables", l.Level), float32(l.NumTables))
			if l.Level == 0 {
				l0 = l.NumTables
			}
		}
		// warn once per approach to the stall threshold
		near := l0 >= s.opts.NumLevelZeroTablesStall*3/4
		if near && !warned {
			s.logger.Warn("level 0 is close to the write stall threshold",
				"level0_tables", l0, "stall_at", s.opts.NumLevelZeroTablesStall)
		}
		warned = near
	}
}

func (s *Store) gauge(name string, v float32) {
	metrics.SetGauge([]string{"nomad", "badger", s.opts.MetricsName, name}, v)
}

func (s *Store) measureSince(name string, start time.Time) {
	metrics.MeasureSince([]string{"nomad", "badger", s.opts.MetricsName, name}, start)
}

// badgerLogger passes Badger's log lines to hclog; its info lines (every
// compaction and flush) are logged at debug level.
type badgerLogger struct{ l hclog.Logger }

func (b badgerLogger) Errorf(f string, a ...any)   { b.l.Error(trim(fmt.Sprintf(f, a...))) }
func (b badgerLogger) Warningf(f string, a ...any) { b.l.Warn(trim(fmt.Sprintf(f, a...))) }
func (b badgerLogger) Infof(f string, a ...any)    { b.l.Debug(trim(fmt.Sprintf(f, a...))) }
func (b badgerLogger) Debugf(f string, a ...any)   { b.l.Trace(trim(fmt.Sprintf(f, a...))) }

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package metricsstore

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/hashicorp/nomad/internal/tstorage"
)

const (
	// DefaultMaxPoints is the number of points of a query by default.
	DefaultMaxPoints = 360

	// MaxPoints is the largest number of points of a query.
	MaxPoints = 2000
)

// Point is the value of a metric in one step of a query: the mean of the
// averages (the sum, for a count such as the OOM kills) and the maximum of
// the maxima of the stored points in the step.
type Point struct {
	// Time is the start of the step.
	Time time.Time
	Avg  float64
	Max  float64
}

// Query returns the points of a metric of a task group between from
// (inclusive) and to (exclusive), downsampled to at most maxPoints steps
// (DefaultMaxPoints when 0). Steps without stored points are left out.
func (s *Store) Query(key GroupKey, metric string, from, to time.Time, maxPoints int) ([]Point, time.Duration, error) {
	idx := metricIndex(metric)
	if idx < 0 {
		return nil, 0, fmt.Errorf("unknown metric %q", metric)
	}
	counter := groupMetricDefs[idx].kind == kindCounter
	if !from.Before(to) {
		return nil, 0, errors.New("from must be before to")
	}
	if maxPoints <= 0 {
		maxPoints = DefaultMaxPoints
	}
	maxPoints = min(maxPoints, MaxPoints)

	step := max(s.cfg.SampleInterval, to.Sub(from)/time.Duration(maxPoints))
	step = step.Truncate(time.Second)
	if to.Sub(from) > step*time.Duration(maxPoints) {
		step += time.Second
	}

	start, end := from.Unix(), to.Unix()
	if start == end {
		end++
	}
	labels := key.labels()
	avg, err := s.selectPoints(metric+".avg", labels, start, end)
	if err != nil {
		return nil, 0, err
	}
	mx, err := s.selectPoints(metric+".max", labels, start, end)
	if err != nil {
		return nil, 0, err
	}

	stepSec := int64(step / time.Second)
	type bucket struct {
		sum   float64
		n     int
		max   float64
		hasMx bool
	}
	buckets := map[int64]*bucket{}
	get := func(ts int64) *bucket {
		k := (ts - start) / stepSec
		b := buckets[k]
		if b == nil {
			b = &bucket{}
			buckets[k] = b
		}
		return b
	}
	for _, p := range avg {
		b := get(p.Timestamp)
		b.sum += p.Value
		b.n++
	}
	for _, p := range mx {
		b := get(p.Timestamp)
		if !b.hasMx || p.Value > b.max {
			b.max, b.hasMx = p.Value, true
		}
	}
	keys := make([]int64, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]Point, 0, len(keys))
	for _, k := range keys {
		b := buckets[k]
		p := Point{Time: time.Unix(start+k*stepSec, 0).UTC(), Max: b.max}
		if b.n > 0 && counter {
			p.Avg = b.sum
		} else if b.n > 0 {
			p.Avg = b.sum / float64(b.n)
		}
		out = append(out, p)
	}
	return out, step, nil
}

// selectPoints reads a series; none is no error.
func (s *Store) selectPoints(metric string, labels []tstorage.Label, start, end int64) ([]*tstorage.DataPoint, error) {
	points, err := s.db.Select(metric, labels, start, end)
	if errors.Is(err, tstorage.ErrNoDataPoints) {
		return nil, nil
	}
	return points, err
}

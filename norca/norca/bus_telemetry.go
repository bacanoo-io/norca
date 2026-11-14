// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/autoscaler"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/metricsstore"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	metrics "github.com/hashicorp/go-metrics"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// telemetryStreamMaxAge is how long the stream keeps the usage reports:
	// a server that was down catches up on this much.
	telemetryStreamMaxAge = time.Hour

	// telemetryStreamWait is how often a server looks for the stream the
	// leader creates.
	telemetryStreamWait = 5 * time.Second

	// telemetryConsumerInactive removes the consumer of a server that is
	// gone.
	telemetryConsumerInactive = 2 * telemetryStreamMaxAge

	// telemetryAckWait is the wait for the acknowledgement of a report
	// before it is delivered again.
	telemetryAckWait = 30 * time.Second

	// telemetryMaxPerNode is how many reports of one node the stream keeps:
	// an hour of reports at the shortest interval, with room for reports
	// split into several messages. A node that publishes faster pushes out
	// its own reports, not those of the other nodes.
	telemetryMaxPerNode = 1024

	// telemetryForeignLogInterval is how often a server logs that reports
	// named groups their node does not run.
	telemetryForeignLogInterval = time.Minute

	// telemetryFreshness is how old a report may be beyond its window to
	// still reach the autoscaler, which takes it as current usage. Older
	// reports, read back after a restart, are skipped.
	telemetryFreshness = time.Minute
)

// keepTelemetryStream creates the stream of the task group usage reports of
// the clients and keeps its settings, e.g. the replicas.
func (s *Server) keepTelemetryStream(ctx context.Context, js jetstream.JetStream) error {
	check := time.NewTicker(busStreamCheckInterval)
	defer check.Stop()
	for {
		if err := s.ensureTelemetryStream(ctx, js); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-check.C:
		}
	}
}

// ensureTelemetryStream creates or updates the telemetry stream: file
// storage, so that the reports survive a restart of every server until they
// are read into the metrics stores, at most a sixteenth of max_storage, the
// oldest reports dropped first.
func (s *Server) ensureTelemetryStream(ctx context.Context, js jetstream.JetStream) error {
	return bus.EnsureStream(ctx, js, bus.TelemetryStreamName(s.config.Region),
		"Norca task group usage of region "+s.config.Region,
		[]string{"norca.telemetry." + structs.BusSubjectToken(s.config.Region) + ".*"},
		bus.StreamSettings{
			Replicas:    s.busReplicas(),
			MaxAge:      telemetryStreamMaxAge,
			MaxBytes:    s.config.Bus.MaxStorage / 16,
			MaxMsgSize:  structs.BusTelemetryMaxMsgSize,
			Storage:     jetstream.FileStorage,
			Compression: jetstream.S2Compression,

			MaxMsgsPerSubject: telemetryMaxPerNode,
		})
}

// consumeTelemetry reads the usage reports of the clients with the durable
// consumer norca-usage-<server id> of this server: every server gets every
// report, also those sent while it restarted (up to the age of the stream).
// A report is acknowledged once it is handled; a report whose acknowledgement
// was lost comes again, and the metrics store takes it once.
func (s *Server) consumeTelemetry(ctx context.Context, js jetstream.JetStream) error {
	name := bus.TelemetryStreamName(s.config.Region)
	for {
		_, err := js.Stream(ctx, name)
		if err == nil {
			break
		}
		if !errors.Is(err, jetstream.ErrStreamNotFound) {
			return err
		}
		// The leader creates the stream.
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(telemetryStreamWait):
		}
	}

	consumer, err := js.CreateOrUpdateConsumer(ctx, name, jetstream.ConsumerConfig{
		Durable:           "norca-usage-" + structs.BusSubjectToken(s.config.NodeID),
		AckPolicy:         jetstream.AckExplicitPolicy,
		AckWait:           telemetryAckWait,
		DeliverPolicy:     jetstream.DeliverAllPolicy,
		InactiveThreshold: telemetryConsumerInactive,
	})
	if err != nil {
		return err
	}
	stopped := make(chan error, 1)
	cc, err := consumer.Consume(func(m jetstream.Msg) {
		s.handleTelemetry(m)
	}, jetstream.ConsumeErrHandler(func(_ jetstream.ConsumeContext, err error) {
		if errors.Is(err, jetstream.ErrConsumerDeleted) || errors.Is(err, jetstream.ErrNoHeartbeat) {
			select {
			case stopped <- err:
			default:
			}
		}
	}))
	if err != nil {
		return err
	}
	defer cc.Stop()

	select {
	case <-ctx.Done():
		return nil
	case err := <-stopped:
		return err
	case <-cc.Closed():
		return errors.New("the telemetry consumer stopped")
	}
}

// autoscalerUsageHistory reads the usage of a task group for the built-in
// autoscaler from the metrics store; without a store no intervals.
func (s *Server) autoscalerUsageHistory(namespace, job, group, metric string, from, to time.Time) (float64, float64, int, error) {
	if s.metricsStore == nil {
		return 0, 0, 0, nil
	}
	key := metricsstore.GroupKey{Namespace: namespace, Job: job, Group: group}
	sum, err := s.metricsStore.Summarize(key, metric, from, to)
	if err != nil {
		return 0, 0, 0, err
	}
	return sum.Avg, sum.Max, sum.Samples, nil
}

// autoscalerGaugeHistory serves the source "norca" of the autoscaler: a
// cluster gauge over a range, the mean of the interval averages and the
// largest maximum.
func (s *Server) autoscalerGaugeHistory(metric string, labels map[string]string, from, to time.Time) (float64, float64, int, error) {
	if s.metricsStore == nil {
		return 0, 0, 0, nil
	}
	points, _, err := s.metricsStore.QueryGauge(metric, labels, from, to, 0)
	if err != nil {
		return 0, 0, 0, err
	}
	var sum, largest float64
	for i, p := range points {
		sum += p.Avg
		if i == 0 || p.Max > largest {
			largest = p.Max
		}
	}
	if len(points) == 0 {
		return 0, 0, 0, nil
	}
	return sum / float64(len(points)), largest, len(points), nil
}

// seedAutoscalerUsage fills the usage window of the built-in autoscaler of a
// new leader from its metrics store, so that the built-in metrics need not
// wait one window after a leader change.
func (s *Server) seedAutoscalerUsage() {
	if s.autoscaler == nil || s.metricsStore == nil {
		return
	}
	windows, err := s.metricsStore.UsageSince(time.Now().Add(-s.autoscaler.MetricsWindow()))
	if err != nil {
		s.logger.Warn("metrics store: reading the usage for the autoscaler failed", "error", err)
		return
	}
	seeds := make([]autoscaler.UsageSeed, len(windows))
	for i, w := range windows {
		seeds[i] = autoscaler.UsageSeed{At: w.Start, Usage: w.Usage}
	}
	if n := s.autoscaler.SeedUsage(seeds); n > 0 {
		s.logger.Info("autoscaler: usage window filled from the metrics store", "intervals", n)
	}
}

// allocCounts returns the allocations of every task group by status, from
// the job summaries, and the task restarts since the previous call.
func (s *Server) allocCounts() (map[metricsstore.GroupKey]metricsstore.AllocCounts, error) {
	snap, err := s.fsm.State().Snapshot()
	if err != nil {
		return nil, err
	}
	iter, err := snap.JobSummaries(nil)
	if err != nil {
		return nil, err
	}
	out := map[metricsstore.GroupKey]metricsstore.AllocCounts{}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		summary := raw.(*structs.JobSummary)
		for group, tg := range summary.Summary {
			key := metricsstore.GroupKey{Namespace: summary.Namespace, Job: summary.JobID, Group: group}
			out[key] = metricsstore.AllocCounts{
				Running:  tg.Running,
				Queued:   tg.Queued,
				Starting: tg.Starting,
				Failed:   tg.Failed,
				Lost:     tg.Lost,
			}
		}
	}
	restarts, err := s.metricsRestartsLast.increase(snap)
	if err != nil {
		return nil, err
	}
	for key, n := range restarts {
		c := out[key]
		c.Restarts = n
		out[key] = c
	}
	return out, nil
}

// recordNodeHost stores the usage of the node of a report, with the
// datacenter, node pool and node class of the node in the state: a report
// can not claim another pool. A node the state does not know is left out.
func (s *Server) recordNodeHost(report *structs.TaskGroupUsageReport, at time.Time) {
	node, err := s.fsm.State().NodeByID(nil, report.NodeID)
	if err != nil || node == nil {
		return
	}
	s.metricsStore.RecordNodeHost(map[string]string{
		"node":  node.ID,
		"dc":    node.Datacenter,
		"pool":  node.NodePool,
		"class": node.NodeClass,
	}, report.Host, at)
}

// keepOwnTaskGroups leaves in a report only the task groups its node has an
// allocation of, running or ended, as the state of this server has them, and
// no more allocations of a group than the node has. The node ID of a report
// is checked against its subject; without this a node could still report the
// usage of any job, move its autoscaler and fill the metrics store with
// series of groups that do not exist. What is left out is counted
// (nomad.bus.telemetry.foreign_groups) and logged once a minute.
func (s *Server) keepOwnTaskGroups(report *structs.TaskGroupUsageReport) {
	if len(report.Groups) == 0 {
		return
	}
	allocs, err := s.fsm.State().AllocsByNode(nil, report.NodeID)
	if err != nil {
		report.Groups = nil
		return
	}
	type groupKey struct{ namespace, job, group string }
	own := make(map[groupKey]int, len(allocs))
	for _, alloc := range allocs {
		own[groupKey{alloc.Namespace, alloc.JobID, alloc.TaskGroup}]++
	}
	kept := report.Groups[:0]
	for _, g := range report.Groups {
		if g == nil {
			continue
		}
		n, ok := own[groupKey{g.Namespace, g.JobID, g.TaskGroup}]
		if !ok {
			continue
		}
		if g.Allocs > n {
			g.Allocs = n
		}
		kept = append(kept, g)
	}
	foreign := len(report.Groups) - len(kept)
	report.Groups = kept
	if foreign == 0 {
		return
	}
	metrics.IncrCounter([]string{"nomad", "bus", "telemetry", "foreign_groups"}, float32(foreign))
	total := s.busTelemetryForeign.Add(uint64(foreign))
	now := time.Now().UnixNano()
	if last := s.busTelemetryForeignLogged.Load(); now-last > int64(telemetryForeignLogInterval) &&
		s.busTelemetryForeignLogged.CompareAndSwap(last, now) {
		s.logger.Warn("system bus: a usage report named task groups its node has no allocation of; they are left out",
			"node", report.NodeID, "groups", foreign, "total", total)
	}
}

// handleTelemetry stores a usage report of the bus in the metrics store and
// hands it to the autoscaler, which keeps it only on the leader. The node of
// the report must be the node of the subject, the only subject the node may
// publish on.
func (s *Server) handleTelemetry(m jetstream.Msg) {
	var report structs.TaskGroupUsageReport
	if err := json.Unmarshal(m.Data(), &report); err != nil {
		s.logger.Debug("system bus: invalid task group usage report", "subject", m.Subject(), "error", err)
		_ = m.Term() // it would not get better by being delivered again
		return
	}
	nodeID := m.Subject()[strings.LastIndex(m.Subject(), ".")+1:]
	if report.NodeID == "" || structs.BusSubjectToken(report.NodeID) != nodeID {
		s.logger.Debug("system bus: ignoring a task group usage report of another node", "subject", m.Subject())
		_ = m.Term()
		return
	}
	// The report is handled by the time it is acknowledged.
	defer func() { _ = m.Ack() }()
	s.keepOwnTaskGroups(&report)

	published := time.Now()
	var sequence uint64
	if meta, err := m.Metadata(); err == nil {
		published = meta.Timestamp
		sequence = meta.Sequence.Stream
	}
	if s.metricsStore != nil {
		if !s.metricsStore.IngestStream(&report, published, sequence) {
			return // delivered again: the autoscaler has it too
		}
		if report.Host != nil {
			s.recordNodeHost(&report, published)
		}
	}
	// The volume usage is in memory: taken again when delivered again.
	s.recordVolumeUsage(&report, published)
	if s.autoscaler == nil || time.Since(published) > report.Window+telemetryFreshness {
		return
	}
	s.autoscaler.RecordUsage(&report)
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/hashicorp/nomad/nomad/autoscaler"
	"github.com/hashicorp/nomad/nomad/bus"
	"github.com/hashicorp/nomad/nomad/metricsstore"
	"github.com/hashicorp/nomad/nomad/structs"
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

// ensureTelemetryStream creates or updates the telemetry stream: memory
// storage (the reports are transient), at most a quarter of max_memory,
// the oldest reports dropped first.
func (s *Server) ensureTelemetryStream(ctx context.Context, js jetstream.JetStream) error {
	return bus.EnsureStream(ctx, js, bus.TelemetryStreamName(s.config.Region),
		"Norca task group usage of region "+s.config.Region,
		[]string{"norca.telemetry." + structs.BusSubjectToken(s.config.Region) + ".*"},
		bus.StreamSettings{
			Replicas:   s.busReplicas(),
			MaxAge:     telemetryStreamMaxAge,
			MaxBytes:   s.config.Bus.MaxMemory / 4,
			MaxMsgSize: structs.BusTelemetryMaxMsgSize,
			Storage:    jetstream.MemoryStorage,
		})
}

// consumeTelemetry reads the usage reports of the clients with the durable
// consumer norca-usage-<server id> of this server: every server gets every
// report, also those sent while it restarted (up to the age of the stream).
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
		AckPolicy:         jetstream.AckNonePolicy,
		DeliverPolicy:     jetstream.DeliverAllPolicy,
		InactiveThreshold: telemetryConsumerInactive,
		MemoryStorage:     true,
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

// allocCounts returns the running and the queued allocations of every task
// group, from the job summaries.
func (s *Server) allocCounts() (map[metricsstore.GroupKey]metricsstore.AllocCounts, error) {
	iter, err := s.fsm.State().JobSummaries(nil)
	if err != nil {
		return nil, err
	}
	out := map[metricsstore.GroupKey]metricsstore.AllocCounts{}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		summary := raw.(*structs.JobSummary)
		for group, tg := range summary.Summary {
			key := metricsstore.GroupKey{Namespace: summary.Namespace, Job: summary.JobID, Group: group}
			out[key] = metricsstore.AllocCounts{Running: tg.Running, Queued: tg.Queued}
		}
	}
	return out, nil
}

// handleTelemetry stores a usage report of the bus in the metrics store and
// hands it to the autoscaler, which keeps it only on the leader. The node of
// the report must be the node of the subject, the only subject the node may
// publish on.
func (s *Server) handleTelemetry(m jetstream.Msg) {
	var report structs.TaskGroupUsageReport
	if err := json.Unmarshal(m.Data(), &report); err != nil {
		s.logger.Debug("system bus: invalid task group usage report", "subject", m.Subject(), "error", err)
		return
	}
	nodeID := m.Subject()[strings.LastIndex(m.Subject(), ".")+1:]
	if report.NodeID == "" || structs.BusSubjectToken(report.NodeID) != nodeID {
		s.logger.Debug("system bus: ignoring a task group usage report of another node", "subject", m.Subject())
		return
	}
	published := time.Now()
	if meta, err := m.Metadata(); err == nil {
		published = meta.Timestamp
	}
	if s.metricsStore != nil {
		s.metricsStore.Ingest(&report, published)
	}
	if s.autoscaler == nil || time.Since(published) > report.Window+telemetryFreshness {
		return
	}
	s.autoscaler.RecordUsage(&report)
}

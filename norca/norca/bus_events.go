// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/stream"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/hashicorp/go-msgpack/codec"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// busRetryMin and busRetryMax bound the backoff of the bus loops.
	busRetryMin = time.Second
	busRetryMax = 30 * time.Second

	// busStreamCheckInterval is how often the leader checks the settings
	// of its streams, e.g. the replicas after servers joined or left.
	busStreamCheckInterval = time.Minute

	// busKVCheckInterval is how often the leader checks the Consul KV
	// bucket: a bus that became clustered starts without it, and the
	// clients need it at once.
	busKVCheckInterval = 10 * time.Second

	// busPublishTimeout bounds the acknowledgement of a batch of events.
	busPublishTimeout = 30 * time.Second
)

// busEventsLeader runs the bus loops of the leader until stopCh closes: the
// key-value bucket of the Consul KV, and with events enabled the event
// stream of the region.
func (s *Server) busEventsLeader(stopCh chan struct{}) {
	if s.bus == nil {
		return
	}
	ctx, cancel := context.WithCancel(s.shutdownCtx)
	go func() {
		select {
		case <-stopCh:
		case <-ctx.Done():
		}
		cancel()
	}()

	var wg sync.WaitGroup
	run := func(what string, fn func(context.Context, jetstream.JetStream) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.busRetryLoop(ctx, what, fn)
		}()
	}
	run("keeping the Consul KV bucket", s.keepConsulKVBucket)
	run("keeping the telemetry stream", s.keepTelemetryStream)
	run("keeping the stream of the Rigger operation logs", s.keepRiggerLogStream)
	run("keeping the object store of the Rigger bundles", s.keepRiggerBundleStore)
	if s.config.Bus.Events.Enabled {
		run("publishing the events", s.publishBusEvents)
	}
	wg.Wait()
}

// keepConsulKVBucket creates the key-value bucket of the Consul KV of the
// compat API and keeps its replicas at one per server, up to three.
func (s *Server) keepConsulKVBucket(ctx context.Context, js jetstream.JetStream) error {
	check := time.NewTicker(busKVCheckInterval)
	defer check.Stop()
	for {
		if err := s.ensureConsulKVBucket(ctx, js); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-check.C:
		}
	}
}

func (s *Server) ensureConsulKVBucket(ctx context.Context, js jetstream.JetStream) error {
	replicas := s.busReplicas()
	kv, err := js.KeyValue(ctx, bus.ConsulKVBucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:      bus.ConsulKVBucket,
			Description: "Consul KV of the Consul-compatible API of Norca",
			History:     1,
			Replicas:    replicas,
			Storage:     jetstream.FileStorage,
			MaxBytes:    s.config.Bus.ConsulKVMaxBytes,
		})
		if errors.Is(err, jetstream.ErrBucketExists) {
			return nil
		}
		if err == nil {
			s.logger.Info("system bus: created the key-value bucket of the Consul KV", "bucket", bus.ConsulKVBucket, "replicas", replicas)
		}
		return err
	}
	if err != nil {
		return err
	}
	status, err := kv.Status(ctx)
	if err != nil {
		return err
	}
	maxBytes := s.config.Bus.ConsulKVMaxBytes
	if bucket, ok := status.(*jetstream.KeyValueBucketStatus); ok {
		if cfg := bucket.StreamInfo().Config; cfg.Replicas == replicas && cfg.MaxBytes == maxBytes {
			return nil
		}
	}
	stream, err := js.Stream(ctx, "KV_"+bus.ConsulKVBucket)
	if err != nil {
		return err
	}
	cfg := stream.CachedInfo().Config
	cfg.Replicas = replicas
	// A bucket made before the limit existed has none, and a limit below
	// what it holds is refused by JetStream: the bucket then keeps its old
	// limit and the error says so.
	cfg.MaxBytes = maxBytes
	_, err = js.UpdateStream(ctx, cfg)
	return err
}

// busRetryLoop runs fn until ctx ends, again after a backoff when it fails.
func (s *Server) busRetryLoop(ctx context.Context, what string, fn func(context.Context, jetstream.JetStream) error) {
	failures := 0
	for ctx.Err() == nil {
		start := time.Now()
		err := s.withBusJetStream(ctx, fn)
		if ctx.Err() != nil {
			return
		}
		// A run that lasted was not part of the failures before it: the
		// backoff starts again (it stayed at its maximum for the rest of the
		// life of the loop).
		if time.Since(start) > 2*busRetryMax {
			failures = 0
		}
		failures++
		s.logger.Warn("system bus: an operation failed, retrying", "operation", what, "error", err)
		wait := min(busRetryMin<<min(failures-1, 5), busRetryMax)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// withBusJetStream waits until JetStream is ready and runs fn with an
// in-process connection.
func (s *Server) withBusJetStream(ctx context.Context, fn func(context.Context, jetstream.JetStream) error) error {
	for !s.bus.JetStreamReady() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	nc, err := s.bus.Connect(nats.NoEcho())
	if err != nil {
		return err
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	return fn(ctx, js)
}

// busReplicas is the number of replicas of the leader's streams.
func (s *Server) busReplicas() int {
	if !s.bus.Clustered() {
		return 1
	}
	return min(max(s.raftServers(), 1), 3)
}

// ensureEventStream creates or updates the event stream and the admin event
// stream of the region; they share bus.events.max_bytes.
func (s *Server) ensureEventStream(ctx context.Context, js jetstream.JetStream) error {
	cfg := s.config.Bus.Events
	region := s.config.Region
	// The change data capture streams have many independent readers: the
	// limits policy (no consumer removes an event for the others), a bound
	// on the consumers, and consumer limits: a consumer idle longer than the
	// events are kept has nothing left to catch up and goes.
	settings := func(maxBytes int64) bus.StreamSettings {
		return bus.StreamSettings{
			Replicas:     s.busReplicas(),
			MaxAge:       cfg.MaxAge,
			MaxBytes:     maxBytes,
			MaxMsgSize:   s.config.Bus.MaxPayload,
			Duplicates:   2 * time.Minute,
			Storage:      jetstream.FileStorage,
			Compression:  jetstream.S2Compression,
			Retention:    jetstream.LimitsPolicy,
			MaxConsumers: cfg.MaxConsumers,
			ConsumerLimits: jetstream.StreamConsumerLimits{
				InactiveThreshold: cfg.MaxAge,
				MaxAckPending:     bus.EventsConsumerMaxAckPending,
			},
		}
	}
	if err := bus.EnsureStream(ctx, js, bus.EventStreamName(region),
		"Norca events of region "+region,
		bus.EventStreamSubjects(cfg.SubjectPrefix, region),
		settings(s.config.Bus.EventStreamMaxBytes())); err != nil {
		return fmt.Errorf("the stream %s: %w", bus.EventStreamName(region), err)
	}
	if err := bus.EnsureStream(ctx, js, bus.AdminEventStreamName(region),
		"Norca admin events of region "+region+" (ACL objects, nodes, audit of the secret store, ...)",
		[]string{cfg.SubjectPrefix + "." + bus.AdminSubjectToken + "." + bus.SubjectToken(region) + ".>"},
		settings(s.config.Bus.AdminEventStreamMaxBytes())); err != nil {
		return fmt.Errorf("the stream %s: %w", bus.AdminEventStreamName(region), err)
	}
	if err := bus.EnsureStream(ctx, js, bus.ConsulKVChangesStreamName(region),
		"Norca Consul KV changes of region "+region+" (keys, never values)",
		bus.ConsulKVChangesSubjects(cfg.SubjectPrefix, region),
		settings(s.config.Bus.ConsulKVChangesMaxBytes())); err != nil {
		return fmt.Errorf("the stream %s: %w", bus.ConsulKVChangesStreamName(region), err)
	}
	return nil
}

// lastBusEvents returns the newest Raft index of the events in the event
// stream and the admin event stream, and the deduplication IDs of the events
// of that index in either of them: the events of one index are one batch
// over both streams.
func lastBusEvents(ctx context.Context, js jetstream.JetStream, region string) (last uint64, stored map[string]bool, found bool, err error) {
	stored = map[string]bool{}
	for _, name := range []string{bus.EventStreamName(region), bus.AdminEventStreamName(region)} {
		st, err := js.Stream(ctx, name)
		if err != nil {
			return 0, nil, false, err
		}
		index, ids, ok, err := bus.LastEvents(ctx, st)
		if err != nil {
			return 0, nil, false, fmt.Errorf("reading the last event of %s: %w", name, err)
		}
		switch {
		case !ok:
		case !found || index > last:
			last, stored, found = index, ids, true
		case index == last:
			for id := range ids {
				stored[id] = true
			}
		}
	}
	return last, stored, found, nil
}

// publishBusEvents publishes the events of the event broker into the event
// streams of the region, from the event after the newest one in them. It
// returns on an error or when ctx ends.
func (s *Server) publishBusEvents(ctx context.Context, js jetstream.JetStream) error {
	cfg := s.config.Bus.Events
	region := s.config.Region
	name := bus.EventStreamName(region)
	if err := s.ensureEventStream(ctx, js); err != nil {
		return fmt.Errorf("creating the event streams: %w", err)
	}
	// The events of one Raft index are a batch, and a batch can end half
	// way (a lost acknowledgement, a leader change): the streams then have
	// some events of their newest index. Continue with that index, without
	// the events of it that are there. Continuing after it lost the rest.
	last, stored, found, err := lastBusEvents(ctx, js, region)
	if err != nil {
		return err
	}

	broker, err := s.State().EventBroker()
	if err != nil {
		return err
	}
	topics := map[structs.Topic][]string{}
	for _, t := range cfg.Topics {
		topics[structs.Topic(t)] = []string{"*"}
	}
	req := &stream.SubscribeRequest{Topics: topics, Namespace: "*"}
	if found {
		req.Index, req.StartExactlyAtIndex = last, true
	}
	sub, err := broker.Subscribe(req)
	if errors.Is(err, stream.ErrIndexNotAvailable) {
		s.logger.Warn("system bus: the event stream can't resume at its newest event, events may be missing; continuing with the newest events",
			"from_index", last)
		stored = nil
		gap := bus.GapMessage(cfg.SubjectPrefix, region, last, "index-gone", time.Now())
		if _, err := js.PublishMsg(ctx, gap); err != nil {
			return fmt.Errorf("publishing the gap marker: %w", err)
		}
		req.Index, req.StartExactlyAtIndex = 0, false
		sub, err = broker.Subscribe(req)
	}
	if err != nil {
		return fmt.Errorf("subscribing to the event broker: %w", err)
	}
	defer sub.Unsubscribe()
	s.logger.Debug("system bus: publishing events", "stream", name, "resume_index", req.Index)

	check := time.NewTicker(busStreamCheckInterval)
	defer check.Stop()
	for {
		select {
		case <-check.C:
			if err := s.ensureEventStream(ctx, js); err != nil {
				s.logger.Warn("system bus: updating the event stream failed", "error", err)
			}
		default:
		}

		nextCtx, cancel := context.WithTimeout(ctx, busStreamCheckInterval)
		events, err := sub.Next(nextCtx)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, context.DeadlineExceeded) {
			continue
		}
		if err != nil {
			return fmt.Errorf("reading the event broker: %w", err)
		}
		if len(events.Events) == 0 {
			continue
		}
		skip := stored
		if events.Index != last {
			skip, stored = nil, nil // past the index the stream ended with
		}
		if err := s.publishEventBatch(ctx, js, cfg.SubjectPrefix, &events, skip); err != nil {
			return err
		}
	}
}

// busEventMessages builds the messages of the events of one Raft index,
// without the events whose deduplication ID is in skip: those the stream
// has from before, when the batch of this index ended half way.
func (s *Server) busEventMessages(prefix string, events *structs.Events, skip map[string]bool) []*nats.Msg {
	msgs := make([]*nats.Msg, 0, len(events.Events))
	for i := range events.Events {
		// Norca: the secrets a job may hold stay out of the bus.
		redacted, isRedacted := busRedactEvent(&events.Events[i])
		ev := &redacted
		raw, err := encodeEventJSON(ev)
		if err != nil {
			s.logger.Warn("system bus: an event can't be encoded, skipped", "topic", ev.Topic, "key", ev.Key, "error", err)
			continue
		}
		busEvent := &bus.Event{
			Index:     ev.Index,
			Topic:     string(ev.Topic),
			Type:      ev.Type,
			Namespace: ev.Namespace,
			Key:       ev.Key,
			JSON:      raw,
			Admin:     busAdminEvent(ev),
			Redacted:  isRedacted,
			Job:       busEventJob(ev),
			Unique:    busEventUnique(ev),
		}
		if skip[bus.EventMsgID(s.config.Region, busEvent)] {
			continue
		}
		msgs = append(msgs, bus.EventMessage(prefix, s.config.Region, busEvent))
	}
	return msgs
}

// publishEventBatch publishes the events of one Raft index and waits for
// their acknowledgements. Republishing after a failure is safe: the stream
// drops duplicates by Nats-Msg-Id within its duplicate window, and skip are
// the IDs of the events of this index that the stream has from before.
func (s *Server) publishEventBatch(ctx context.Context, js jetstream.JetStream, prefix string, events *structs.Events, skip map[string]bool) error {
	msgs := s.busEventMessages(prefix, events, skip)
	futures := make([]jetstream.PubAckFuture, 0, len(msgs))
	for _, msg := range msgs {
		f, err := js.PublishMsgAsync(msg)
		if err != nil {
			return fmt.Errorf("publishing the events of index %d: %w", events.Index, err)
		}
		futures = append(futures, f)
	}
	timeout := time.NewTimer(busPublishTimeout)
	defer timeout.Stop()
	for _, f := range futures {
		select {
		case <-f.Ok():
		case err := <-f.Err():
			return fmt.Errorf("publishing the events of index %d: %w", events.Index, err)
		case <-timeout.C:
			return fmt.Errorf("publishing the events of index %d: no acknowledgement within %s", events.Index, busPublishTimeout)
		case <-ctx.Done():
			return nil
		}
	}
	return nil
}

// encodeEventJSON encodes an event as the event stream API does.
func encodeEventJSON(ev *structs.Event) ([]byte, error) {
	var buf bytes.Buffer
	if err := codec.NewEncoder(&buf, structs.JsonHandleWithExtensions).Encode(ev); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

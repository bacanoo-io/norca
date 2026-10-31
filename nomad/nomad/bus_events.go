// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/go-msgpack/codec"
	"github.com/hashicorp/nomad/nomad/bus"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/stream"
	"github.com/hashicorp/nomad/nomad/structs"
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

	// pushStreamName and its age limit; bus.PushStreamMaxBytes bounds its
	// size.
	pushStreamName   = "NORCA_PACK_PUSHES"
	pushStreamMaxAge = 24 * time.Hour

	// pushSyncCooldown is the minimum time between two syncs of a registry
	// triggered by push notices.
	pushSyncCooldown = time.Minute
)

// busEventsLeader runs the bus loops of the leader until stopCh closes: the
// key-value bucket of the Consul KV, and with events enabled the event
// stream of the region and the pack push notices.
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
	if s.config.Bus.Events.Enabled {
		run("publishing the events", s.publishBusEvents)
		run("consuming the pack push notices", s.consumePushNotices)
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
	if status.(*jetstream.KeyValueBucketStatus).StreamInfo().Config.Replicas == replicas {
		return nil
	}
	stream, err := js.Stream(ctx, "KV_"+bus.ConsulKVBucket)
	if err != nil {
		return err
	}
	cfg := stream.CachedInfo().Config
	cfg.Replicas = replicas
	_, err = js.UpdateStream(ctx, cfg)
	return err
}

// busRetryLoop runs fn until ctx ends, again after a backoff when it fails.
func (s *Server) busRetryLoop(ctx context.Context, what string, fn func(context.Context, jetstream.JetStream) error) {
	failures := 0
	for ctx.Err() == nil {
		err := s.withBusJetStream(ctx, fn)
		if ctx.Err() != nil {
			return
		}
		failures++
		s.logger.Warn("system bus: "+what+" failed, retrying", "error", err)
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

func (s *Server) ensureEventStream(ctx context.Context, js jetstream.JetStream) error {
	cfg := s.config.Bus.Events
	return bus.EnsureStream(ctx, js, bus.EventStreamName(s.config.Region),
		"Norca events of region "+s.config.Region,
		[]string{cfg.SubjectPrefix + "." + bus.SubjectToken(s.config.Region) + ".>"},
		bus.StreamSettings{
			Replicas:    s.busReplicas(),
			MaxAge:      cfg.MaxAge,
			MaxBytes:    cfg.MaxBytes,
			MaxMsgSize:  s.config.Bus.MaxPayload,
			Duplicates:  2 * time.Minute,
			Storage:     jetstream.FileStorage,
			Compression: jetstream.S2Compression,
		})
}

// publishBusEvents publishes the events of the event broker into the event
// stream of the region, from the event after the newest one in the stream.
// It returns on an error or when ctx ends.
func (s *Server) publishBusEvents(ctx context.Context, js jetstream.JetStream) error {
	cfg := s.config.Bus.Events
	region := s.config.Region
	name := bus.EventStreamName(region)
	if err := s.ensureEventStream(ctx, js); err != nil {
		return fmt.Errorf("creating the stream %s: %w", name, err)
	}
	eventStream, err := js.Stream(ctx, name)
	if err != nil {
		return err
	}
	last, found, err := bus.LastEventIndex(ctx, eventStream)
	if err != nil {
		return fmt.Errorf("reading the last event of %s: %w", name, err)
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
		req.Index, req.StartExactlyAtIndex = last+1, true
	}
	sub, err := broker.Subscribe(req)
	if errors.Is(err, stream.ErrIndexNotAvailable) {
		s.logger.Warn("system bus: the event stream can't resume after its newest event, events may be missing; continuing with the newest events",
			"from_index", last+1)
		gap := bus.GapMessage(cfg.SubjectPrefix, region, last+1, "index-gone", time.Now())
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
		if err := s.publishEventBatch(ctx, js, cfg.SubjectPrefix, &events); err != nil {
			return err
		}
	}
}

// publishEventBatch publishes the events of one Raft index and waits for
// their acknowledgements. Republishing after a failure is safe: the stream
// drops duplicates by Nats-Msg-Id.
func (s *Server) publishEventBatch(ctx context.Context, js jetstream.JetStream, prefix string, events *structs.Events) error {
	futures := make([]jetstream.PubAckFuture, 0, len(events.Events))
	for i := range events.Events {
		ev := &events.Events[i]
		raw, err := encodeEventJSON(ev)
		if err != nil {
			s.logger.Warn("system bus: an event can't be encoded, skipped", "topic", ev.Topic, "key", ev.Key, "error", err)
			continue
		}
		msg := bus.EventMessage(prefix, s.config.Region, &bus.Event{
			Index:     ev.Index,
			Topic:     string(ev.Topic),
			Type:      ev.Type,
			Namespace: ev.Namespace,
			Key:       ev.Key,
			JSON:      raw,
		})
		f, err := js.PublishMsgAsync(msg)
		if err != nil {
			return fmt.Errorf("publishing event %d: %w", ev.Index, err)
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

// ensurePushStream creates the stream of the pack push notices.
func (s *Server) ensurePushStream(ctx context.Context, js jetstream.JetStream) error {
	prefix := s.config.Bus.Events.SubjectPrefix
	return bus.EnsureStream(ctx, js, pushStreamName, "Norca pack push notices",
		[]string{prefix + "." + bus.PushSubject, prefix + "." + bus.PushSubject + ".>"},
		bus.StreamSettings{
			Replicas:   s.busReplicas(),
			MaxAge:     pushStreamMaxAge,
			MaxBytes:   bus.PushStreamMaxBytes,
			MaxMsgSize: 64 << 10,
			Duplicates: time.Minute,
			Storage:    jetstream.FileStorage,
		})
}

// consumePushNotices syncs the registries named by pack push notices. The
// leader reads them with the durable consumer norca-sync-<region>, so
// notices published during a leader change are handled by the next leader.
func (s *Server) consumePushNotices(ctx context.Context, js jetstream.JetStream) error {
	if err := s.ensurePushStream(ctx, js); err != nil {
		return fmt.Errorf("creating the stream %s: %w", pushStreamName, err)
	}
	consumer, err := js.CreateOrUpdateConsumer(ctx, pushStreamName, jetstream.ConsumerConfig{
		Durable:           "norca-sync-" + bus.SubjectToken(s.config.Region),
		AckPolicy:         jetstream.AckExplicitPolicy,
		AckWait:           30 * time.Second,
		MaxDeliver:        10,
		MaxAckPending:     100,
		InactiveThreshold: 7 * 24 * time.Hour,
		Replicas:          s.busReplicas(),
	})
	if err != nil {
		return fmt.Errorf("creating the push notice consumer: %w", err)
	}
	syncer := &pushSyncer{srv: s, last: map[string]time.Time{}}
	cc, err := consumer.Consume(func(m jetstream.Msg) {
		var n bus.PushNotice
		if err := json.Unmarshal(m.Data(), &n); err != nil {
			s.logger.Warn("system bus: invalid pack push notice", "subject", m.Subject(), "error", err)
			_ = m.Term()
			return
		}
		if err := syncer.handle(&n); err != nil {
			s.logger.Warn("system bus: pack push notice not handled", "error", err)
			_ = m.NakWithDelay(30 * time.Second)
			return
		}
		_ = m.Ack()
	})
	if err != nil {
		return err
	}
	defer cc.Stop()

	check := time.NewTicker(busStreamCheckInterval)
	defer check.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-cc.Closed():
			return errors.New("the push notice consumer stopped")
		case <-check.C:
			if err := s.ensurePushStream(ctx, js); err != nil {
				s.logger.Warn("system bus: updating the push notice stream failed", "error", err)
			}
		}
	}
}

// pushSyncer syncs the registries matching push notices, at most once per
// cooldown each.
type pushSyncer struct {
	srv  *Server
	mu   sync.Mutex
	last map[string]time.Time
}

// handle syncs the registries whose name or source matches the notice. It
// fails only when the registries can't be read.
func (p *pushSyncer) handle(n *bus.PushNotice) error {
	if n.Source == "" && n.Registry == "" {
		return nil
	}
	snap, err := p.srv.State().Snapshot()
	if err != nil {
		return err
	}
	iter, err := snap.CustomObjects(nil, structs.ReleaseObjectGroup, structs.PackRegistryKind, "", state.SortDefault)
	if err != nil {
		return err
	}
	source := bus.NormalizeSource(n.Source)
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		obj := raw.(*structs.CustomObject)
		reg, err := packRegistryFromObject(obj)
		if err != nil {
			continue
		}
		if !(n.Registry != "" && reg.Name == n.Registry) &&
			!(source != "" && reg.Spec != nil && bus.NormalizeSource(reg.Spec.Source) == source) {
			continue
		}
		p.mu.Lock()
		if t, ok := p.last[reg.Name]; ok && time.Since(t) < pushSyncCooldown {
			p.mu.Unlock()
			p.srv.logger.Debug("pack registry sync on push skipped: cooldown", "registry", reg.Name)
			continue
		}
		p.last[reg.Name] = time.Now()
		p.mu.Unlock()
		if _, _, err := p.srv.syncPackRegistryObject(obj); err != nil {
			p.srv.logger.Warn("pack registry sync on push failed", "registry", reg.Name, "error", err)
			continue
		}
		p.srv.logger.Info("pack registry synced on push", "registry", reg.Name)
	}
	return nil
}

// publishPushNotice publishes a push notice on the bus; without the bus it
// syncs the matching registries of this region directly.
func (s *Server) publishPushNotice(n *bus.PushNotice) error {
	if s.bus == nil || !s.config.Bus.Events.Enabled {
		return (&pushSyncer{srv: s, last: map[string]time.Time{}}).handle(n)
	}
	data, err := json.Marshal(n)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(s.shutdownCtx, 10*time.Second)
	defer cancel()
	return s.withBusJetStream(ctx, func(ctx context.Context, js jetstream.JetStream) error {
		if err := s.ensurePushStream(ctx, js); err != nil {
			return err
		}
		_, err := js.Publish(ctx, s.config.Bus.Events.SubjectPrefix+"."+bus.PushSubject, data)
		return err
	})
}

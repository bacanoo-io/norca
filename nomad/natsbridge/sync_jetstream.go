// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package natsbridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// The inbound mode with JetStream reads its messages through durable
// consumers, so that the push notices and the registry events of other
// regions published while no bridge instance of the region runs are handled
// when one starts again:
//
//   - push notices are stored in the stream DefaultPushStream (its own
//     stream: no region stream holds <prefix>.packs.pushed), shared by all
//     regions; every region has its consumer on it;
//   - the registry events of another region are in that region's event
//     stream (the stream of <prefix>.<region>.>); the bridge finds these
//     streams and creates its consumer on each, filtered to
//     <prefix>.<region>.CustomObject.
//
// The bridge instances of a region share the consumers, so one of them
// handles each message. A new consumer starts with the messages published
// after its creation; it is removed by the server after
// SyncConsumerInactive without a bridge.

// DefaultPushStream is the default name of the stream of the push notices.
const DefaultPushStream = "NORCA_PACK_PUSHES"

const (
	// pushStreamMaxAge and pushStreamMaxBytes limit the push notice stream:
	// a notice only makes a sync come sooner.
	pushStreamMaxAge   = 24 * time.Hour
	pushStreamMaxBytes = 64 << 20

	// SyncConsumerInactive is the time after which the server removes a
	// consumer of a region whose bridges are gone.
	SyncConsumerInactive = 7 * 24 * time.Hour

	syncMaxDeliver = 10
)

// Variables for the tests:
var (
	// syncAckWait is the time after which an unacknowledged message is
	// redelivered. It also bounds the delay after a bridge restart: the
	// server may still deliver to the pull requests of the stopped instance
	// until they expire.
	syncAckWait = 30 * time.Second
	// syncRetryDelay is the delay of the redelivery of a message whose
	// handling failed because the Norca API was not reachable.
	syncRetryDelay = 30 * time.Second
)

// EnsurePushStream creates the stream of the push notices
// (<prefix>.packs.pushed and <prefix>.packs.pushed.>), or updates the
// settings of an existing one that can change. It keeps the notices for a
// day; the replicas, storage and compression follow set.
func EnsurePushStream(ctx context.Context, js jetstream.JetStream, name, prefix string, set StreamSettings) (StreamResult, error) {
	set.MaxAge = pushStreamMaxAge
	set.MaxBytes = pushStreamMaxBytes
	set.Duplicates = min(max(set.Duplicates, time.Minute), set.MaxAge)
	set.DenyDelete, set.DenyPurge = false, false
	return ensureStream(ctx, js, jetstream.StreamConfig{
		Name:        name,
		Description: "Norca pack push notices",
		Subjects:    []string{prefix + "." + PushSubject, prefix + "." + PushSubject + ".>"},
	}, set)
}

// SyncConsumerOptions configure Syncer.Consume.
type SyncConsumerOptions struct {
	Prefix string
	// Pushes consumes the push notices of PushStream.
	Pushes     bool
	PushStream string
	// Regions consumes the registry events of the event streams of the
	// other regions, looking for new ones every Discover (default 1m).
	Regions  bool
	Discover time.Duration
}

// syncConsumer runs the durable consumers of a syncer.
type syncConsumer struct {
	s    *Syncer
	js   jetstream.JetStream
	opts SyncConsumerOptions
	name string

	mu       sync.Mutex
	running  map[string]jetstream.ConsumeContext // by stream
	notFound bool                                // the push stream was missing last time
}

// Consume handles the push notices and registry events through durable
// JetStream consumers of the region until ctx is canceled. Streams that are
// missing or unreachable are retried every Discover.
func (s *Syncer) Consume(ctx context.Context, js jetstream.JetStream, opts SyncConsumerOptions) {
	if opts.Discover <= 0 {
		opts.Discover = time.Minute
	}
	c := &syncConsumer{
		s:       s,
		js:      js,
		opts:    opts,
		name:    "norca-sync-" + consumerToken(s.Region),
		running: map[string]jetstream.ConsumeContext{},
	}
	defer c.stopAll()
	t := time.NewTicker(opts.Discover)
	defer t.Stop()
	for {
		c.discover(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *syncConsumer) discover(ctx context.Context) {
	if c.opts.Pushes {
		err := c.start(ctx, c.opts.PushStream, "", c.handlePush)
		missing := errors.Is(err, jetstream.ErrStreamNotFound)
		switch {
		case missing && !c.notFound:
			c.s.logger().Warn("the push notice stream does not exist; create it (-jetstream-create) or the push notices are not stored",
				"stream", c.opts.PushStream)
		case err != nil && !missing:
			c.s.logger().Warn("consuming the push notices failed", "stream", c.opts.PushStream, "error", err)
		}
		c.notFound = missing
	}
	if c.opts.Regions {
		c.discoverRegions(ctx)
	}
}

// discoverRegions starts a consumer on the event stream of every other
// region that holds registry events.
func (c *syncConsumer) discoverRegions(ctx context.Context) {
	names := c.js.StreamNames(ctx, jetstream.WithStreamListSubject(c.opts.Prefix+".*.CustomObject"))
	for name := range names.Name() {
		if c.isRunning(name) {
			continue
		}
		region, ok := c.streamRegion(ctx, name)
		if !ok || region == subjectToken(c.s.Region) {
			continue
		}
		filter := c.opts.Prefix + "." + region + ".CustomObject"
		if err := c.start(ctx, name, filter, c.handleRegistryEvent); err != nil {
			c.s.logger().Warn("consuming the registry events of a region failed", "stream", name, "region", region, "error", err)
		}
	}
	if err := names.Err(); err != nil && !errors.Is(err, context.Canceled) {
		c.s.logger().Warn("listing the event streams of the regions failed", "error", err)
	}
}

// streamRegion returns the region token of an event stream, the stream of
// <prefix>.<region>.>.
func (c *syncConsumer) streamRegion(ctx context.Context, name string) (string, bool) {
	s, err := c.js.Stream(ctx, name)
	if err != nil {
		return "", false
	}
	info := s.CachedInfo()
	for _, subj := range info.Config.Subjects {
		rest, ok := strings.CutPrefix(subj, c.opts.Prefix+".")
		if !ok {
			continue
		}
		region, tail, _ := strings.Cut(rest, ".")
		if region != "" && region != "*" && region != ">" && region != "packs" && (tail == ">" || tail == "CustomObject") {
			return region, true
		}
	}
	return "", false
}

func (c *syncConsumer) isRunning(stream string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.running[stream]
	return ok
}

// start creates or updates the durable consumer of the region on the stream
// and consumes it, unless it already runs.
func (c *syncConsumer) start(ctx context.Context, stream, filter string, handle func(context.Context, jetstream.Msg)) error {
	if c.isRunning(stream) {
		return nil
	}
	cons, err := c.js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable:           c.name,
		Description:       "Pack registry syncs of region " + c.s.Region + " (norca-natsbridge)",
		FilterSubject:     filter,
		DeliverPolicy:     jetstream.DeliverNewPolicy,
		AckPolicy:         jetstream.AckExplicitPolicy,
		AckWait:           syncAckWait,
		MaxDeliver:        syncMaxDeliver,
		InactiveThreshold: SyncConsumerInactive,
	})
	if err != nil {
		return err
	}
	cc, err := cons.Consume(func(m jetstream.Msg) { handle(ctx, m) },
		jetstream.ConsumeErrHandler(func(_ jetstream.ConsumeContext, err error) {
			// A deleted consumer (or stream) stops consuming; the next
			// discovery creates it again.
			if errors.Is(err, jetstream.ErrConsumerDeleted) || errors.Is(err, jetstream.ErrConsumerNotFound) {
				c.s.logger().Warn("pack registry sync consumer removed; recreating it", "stream", stream, "error", err)
				c.forget(stream)
			}
		}))
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.running[stream] = cc
	c.mu.Unlock()
	c.s.logger().Info("consuming pack registry sync messages", "stream", stream, "consumer", c.name, "filter", filter)
	return nil
}

func (c *syncConsumer) forget(stream string) {
	c.mu.Lock()
	cc, ok := c.running[stream]
	delete(c.running, stream)
	c.mu.Unlock()
	if ok {
		cc.Stop()
	}
}

func (c *syncConsumer) stopAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for stream, cc := range c.running {
		cc.Stop()
		delete(c.running, stream)
	}
}

// settle acknowledges a handled message, redelivers it later when the Norca
// API failed, and drops it when it can't be handled at all.
func (c *syncConsumer) settle(m jetstream.Msg, what string, invalid bool, err error) {
	switch {
	case invalid:
		c.s.logger().Warn("invalid "+what+" dropped", "subject", m.Subject(), "error", err)
		_ = m.Term()
	case err != nil:
		c.s.logger().Warn(what+" not handled, retrying later", "subject", m.Subject(), "error", err)
		_ = m.NakWithDelay(syncRetryDelay)
	default:
		_ = m.Ack()
	}
}

func (c *syncConsumer) handlePush(ctx context.Context, m jetstream.Msg) {
	var n PushNotice
	if err := json.Unmarshal(m.Data(), &n); err != nil {
		c.settle(m, "push notice", true, err)
		return
	}
	if n.Source == "" && n.Registry == "" {
		c.settle(m, "push notice", true, errors.New("names neither a source nor a registry"))
		return
	}
	_, err := c.s.HandlePush(ctx, &n)
	c.settle(m, "push notice", false, err)
}

func (c *syncConsumer) handleRegistryEvent(ctx context.Context, m jetstream.Msg) {
	if !json.Valid(m.Data()) {
		c.settle(m, "registry event", true, errors.New("not JSON"))
		return
	}
	_, err := c.s.HandleRegistryEvent(ctx, m.Data())
	c.settle(m, "registry event", false, err)
}

// consumerToken makes a region usable in a consumer name, which allows
// neither the subject wildcards nor dots, slashes or white space.
func consumerToken(s string) string {
	if s == "" {
		return "_"
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '.', '*', '>', '/', '\\', ' ', '\t', '\r', '\n':
			return '_'
		}
		return r
	}, s)
}

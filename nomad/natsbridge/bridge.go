// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package natsbridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hashicorp/nomad/controller"
	"github.com/nats-io/nats.go"
)

// Start positions of a bridge without a checkpoint.
const (
	// StartNow starts with the newest events.
	StartNow = "now"

	// StartOldest starts with the oldest events still in the event buffer
	// of the server.
	StartOldest = "oldest"
)

// Options configures a Bridge.
type Options struct {
	// Region is the region of the event stream, used in the subjects and
	// the payloads.
	Region string

	// SubjectPrefix is the first part of the subjects (default "nomad").
	SubjectPrefix string

	// StartAt is where a bridge without a checkpoint starts: StartNow
	// (default) or StartOldest.
	StartAt string

	// EventTypes, if not empty, publishes only the events of these types
	// (client-side filter on top of the topic filters).
	EventTypes map[string]bool

	// Skip, if set, drops the events it returns true for, for example the
	// writes of the bridge's own checkpoint and lease Variables.
	Skip func(ev *Event) bool

	// CheckpointInterval is the minimum time between checkpoint writes
	// after events were published (default 1s). A checkpoint is also
	// written when the bridge stops.
	CheckpointInterval time.Duration

	// CheckpointIdleInterval is the minimum time between checkpoint writes
	// when the index advanced only through skipped events (default 1m).
	// Such writes keep the checkpoint inside the event buffer of the
	// server; writing them more often would make the bridge's own
	// checkpoint writes a busy loop.
	CheckpointIdleInterval time.Duration

	// RetryBase and RetryMax bound the backoff after errors (defaults 1s
	// and 30s).
	RetryBase time.Duration
	RetryMax  time.Duration
}

// Bridge forwards the event stream to NATS.
type Bridge struct {
	opts    Options
	src     EventSource
	pub     Publisher
	cp      Checkpointer
	metrics *Metrics
	logger  *slog.Logger
	now     func() time.Time
}

// New returns a bridge. metrics and logger may be nil.
func New(src EventSource, pub Publisher, cp Checkpointer, opts Options, metrics *Metrics, logger *slog.Logger) (*Bridge, error) {
	if opts.Region == "" {
		return nil, errors.New("the region is required")
	}
	if opts.SubjectPrefix == "" {
		opts.SubjectPrefix = "nomad"
	}
	if err := validPrefix(opts.SubjectPrefix); err != nil {
		return nil, err
	}
	switch opts.StartAt {
	case "":
		opts.StartAt = StartNow
	case StartNow, StartOldest:
	default:
		return nil, fmt.Errorf("invalid start position %q (want %q or %q)", opts.StartAt, StartNow, StartOldest)
	}
	if opts.CheckpointInterval <= 0 {
		opts.CheckpointInterval = time.Second
	}
	if opts.CheckpointIdleInterval <= 0 {
		opts.CheckpointIdleInterval = time.Minute
	}
	if opts.RetryBase <= 0 {
		opts.RetryBase = time.Second
	}
	if opts.RetryMax <= 0 {
		opts.RetryMax = 30 * time.Second
	}
	if metrics == nil {
		metrics = NewMetrics()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Bridge{
		opts: opts, src: src, pub: pub, cp: cp, metrics: metrics,
		logger: logger.With("region", opts.Region),
		now:    time.Now,
	}, nil
}

// SkipVariables returns a Skip function that drops the Variables events of
// the given paths in namespace.
func SkipVariables(namespace string, paths ...string) func(ev *Event) bool {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		if p != "" {
			set[p] = true
		}
	}
	return func(ev *Event) bool {
		return ev.Topic == "Variables" && ev.Namespace == namespace && set[ev.Key]
	}
}

// position is the state of the stream of a running bridge.
type position struct {
	// next is the index to resume from; 0 means from the newest events.
	next uint64

	// exact is set when next must be resumed exactly: it follows a
	// checkpoint or a published batch.
	exact bool

	// last is the index of the last batch handled (published or skipped),
	// saved is the index of the stored checkpoint.
	last, saved uint64

	// dirty is set when events were published after the checkpoint.
	dirty    bool
	savedAt  time.Time
	hasSaved bool
}

// Run forwards events until ctx is canceled. It returns nil then, after
// storing the checkpoint; it returns an error only if the checkpoint
// cannot be loaded before ctx is canceled.
func (b *Bridge) Run(ctx context.Context) error {
	pos, err := b.load(ctx)
	if err != nil {
		return err
	}
	b.logger.Info("bridge started", "resume_index", pos.next, "exact", pos.exact)
	defer func() {
		saveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		b.checkpoint(saveCtx, &pos, true)
		b.logger.Info("bridge stopped", "checkpoint", pos.saved)
	}()

	failures := 0
	for ctx.Err() == nil {
		ch, err := b.src.Stream(ctx, pos.next, pos.exact)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, ErrIndexGone) {
				if err := b.handleGone(ctx, &pos); err != nil && ctx.Err() == nil {
					failures++
					b.logger.Warn("publishing the gap marker failed", "error", err)
					b.sleep(ctx, failures)
				}
				continue
			}
			failures++
			b.metrics.streamError("subscribe")
			b.logger.Warn("subscribing to the event stream failed", "index", pos.next, "error", err)
			b.sleep(ctx, failures)
			continue
		}
		b.logger.Debug("subscribed", "index", pos.next, "exact", pos.exact)

		published, err := b.consume(ctx, ch, &pos)
		if ctx.Err() != nil {
			return nil
		}
		if published {
			failures = 0
		}
		if err != nil {
			failures++
			b.logger.Warn("publishing failed, resubscribing", "index", pos.next, "error", err)
		} else {
			b.logger.Debug("event stream ended, resubscribing", "index", pos.next)
		}
		// The exact resume index guarantees that nothing is lost while
		// reconnecting.
		b.sleep(ctx, max(failures, 1))
	}
	return nil
}

// load reads the checkpoint, retrying until it succeeds or ctx is canceled.
func (b *Bridge) load(ctx context.Context) (position, error) {
	for failures := 1; ; failures++ {
		index, found, err := b.cp.Load(ctx)
		if err == nil {
			pos := position{}
			switch {
			case found:
				pos.next, pos.exact = index+1, true
				pos.last, pos.saved, pos.hasSaved = index, index, true
				pos.savedAt = b.now()
				b.metrics.checkpointed(index)
				b.metrics.publishedIndex(index)
			case b.opts.StartAt == StartOldest:
				pos.next = 1
			}
			return pos, nil
		}
		if ctx.Err() != nil {
			return position{}, fmt.Errorf("loading the checkpoint: %w", err)
		}
		b.metrics.checkpointFailed()
		b.logger.Warn("loading the checkpoint failed", "error", err)
		b.sleep(ctx, failures)
	}
}

// handleGone publishes a gap marker and continues with the newest events.
func (b *Bridge) handleGone(ctx context.Context, pos *position) error {
	from := pos.next
	b.logger.Warn("the event stream cannot resume from the checkpoint, events may be lost; continuing with the newest events",
		"from_index", from)
	msg := gapMessage(b.opts.SubjectPrefix, b.opts.Region, from, "index-gone", b.now())
	if _, err := b.pub.PublishBatch(ctx, []*nats.Msg{msg}); err != nil {
		b.metrics.publishFailed(1)
		return err
	}
	b.metrics.gap()
	b.metrics.published(GapTopic, 1, 0)
	pos.next, pos.exact = 0, false
	return nil
}

// consume publishes the batches of one subscription. It returns when the
// stream ends, a publish fails (with the error) or ctx is canceled;
// published reports whether at least one batch was handled.
func (b *Bridge) consume(ctx context.Context, ch <-chan Batch, pos *position) (published bool, err error) {
	ticker := time.NewTicker(b.opts.CheckpointInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return published, nil
		case <-ticker.C:
			b.checkpoint(ctx, pos, false)
		case batch, ok := <-ch:
			if !ok {
				return published, nil
			}
			if batch.Err != nil {
				b.metrics.streamError("interrupted")
				b.logger.Debug("event stream interrupted", "error", batch.Err)
				return published, nil
			}
			if pos.exact && batch.Index < pos.next {
				// Already handled.
				continue
			}
			n, err := b.publish(ctx, &batch)
			if err != nil {
				return published, err
			}
			published = true
			pos.next, pos.exact, pos.last = batch.Index+1, true, batch.Index
			pos.dirty = pos.dirty || n > 0
			b.checkpoint(ctx, pos, false)
		}
	}
}

// publish publishes the events of a batch that pass the filters and
// returns their number.
func (b *Bridge) publish(ctx context.Context, batch *Batch) (int, error) {
	b.metrics.received(batch.Index)
	msgs := make([]*nats.Msg, 0, len(batch.Events))
	topics := make([]string, 0, len(batch.Events))
	for i := range batch.Events {
		ev := &batch.Events[i]
		switch {
		case b.opts.Skip != nil && b.opts.Skip(ev):
			b.metrics.skipped("own")
			continue
		case len(b.opts.EventTypes) > 0 && !b.opts.EventTypes[ev.Type]:
			b.metrics.skipped("type")
			continue
		}
		msgs = append(msgs, eventMessage(b.opts.SubjectPrefix, b.opts.Region, ev))
		topics = append(topics, ev.Topic)
	}
	duplicates, err := b.pub.PublishBatch(ctx, msgs)
	if err != nil {
		if ctx.Err() == nil {
			b.metrics.publishFailed(len(msgs))
		}
		return 0, err
	}
	for _, topic := range topics {
		b.metrics.published(topic, 1, 0)
	}
	if duplicates > 0 {
		b.metrics.published("", 0, duplicates)
	}
	b.metrics.publishedIndex(batch.Index)
	return len(msgs), nil
}

// checkpoint stores the position if it is due, or always with force.
func (b *Bridge) checkpoint(ctx context.Context, pos *position, force bool) {
	if pos.last == 0 || (pos.hasSaved && pos.last <= pos.saved) {
		return
	}
	since := b.now().Sub(pos.savedAt)
	due := force ||
		(pos.dirty && since >= b.opts.CheckpointInterval) ||
		(!pos.hasSaved || since >= b.opts.CheckpointIdleInterval)
	if !due {
		return
	}
	if err := b.cp.Save(ctx, pos.last); err != nil {
		b.metrics.checkpointFailed()
		b.logger.Warn("storing the checkpoint failed", "index", pos.last, "error", err)
		return
	}
	pos.saved, pos.savedAt, pos.hasSaved, pos.dirty = pos.last, b.now(), true, false
	b.metrics.checkpointed(pos.last)
}

func (b *Bridge) sleep(ctx context.Context, failures int) {
	d := b.opts.RetryBase
	for i := 1; i < failures && d < b.opts.RetryMax; i++ {
		d *= 2
	}
	t := time.NewTimer(min(d, b.opts.RetryMax))
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// RunElected runs the bridge only while le holds the leadership, so that of
// several instances one publishes and the others stand by. It returns when
// ctx is canceled.
func (b *Bridge) RunElected(ctx context.Context, le *controller.LeaderElector) {
	le.Run(ctx, func(leaderCtx context.Context) {
		b.metrics.SetLeader(true)
		b.logger.Info("became the leader, publishing")
		if err := b.Run(leaderCtx); err != nil && leaderCtx.Err() == nil {
			b.logger.Error("bridge failed", "error", err)
		}
	}, func() {
		b.metrics.SetLeader(false)
		b.logger.Info("lost the leadership, standing by")
	})
}

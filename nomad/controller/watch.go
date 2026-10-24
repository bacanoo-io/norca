// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
)

// WatchMode selects how changes are watched.
type WatchMode string

const (
	// WatchAuto uses the event stream, and falls back to polling with
	// blocking queries if the token may not subscribe to the event stream.
	// (In Nomad 1.6.5 the event stream accepts only ACL tokens, not
	// workload identities.)
	WatchAuto WatchMode = ""

	// WatchStream uses only the event stream.
	WatchStream WatchMode = "stream"

	// WatchPoll uses only blocking queries.
	WatchPoll WatchMode = "poll"
)

// watchConfig describes one list + watch subscription.
type watchConfig struct {
	// name identifies the watch in logs and metrics.
	name      string
	namespace string
	topics    map[api.Topic][]string

	// list synchronizes the local state from a full listing and returns
	// the index of the listing; the event stream is resumed after it.
	list func(ctx context.Context) (uint64, error)

	// apply applies one event of the stream. An error makes the watch list
	// again, so a failed read does not lose the change.
	apply func(ctx context.Context, ev *api.Event) error

	// poll, if set, waits for changes after index with a blocking query,
	// synchronizes the local state and returns the new index. It is used
	// instead of the event stream in WatchPoll mode, or in WatchAuto mode
	// when the stream is not permitted.
	poll func(ctx context.Context, index uint64) (uint64, error)

	// resyncPeriod, if positive, forces a full listing periodically even
	// if the stream works, as a safety net.
	resyncPeriod time.Duration

	// fromNowAfterGone makes the stream start from the newest events after
	// a 410 Gone instead of from the listing index (see run).
	fromNowAfterGone bool
}

// streamForbidden reports whether the event stream refused the token. The
// server reports a token it cannot resolve for the event stream (for example
// a workload identity) or a missing permission as 500 with the ACL error
// message, so those messages are recognized too.
func streamForbidden(err error) bool {
	switch statusCode(err) {
	case http.StatusForbidden:
		return true
	case http.StatusInternalServerError:
		msg := err.Error()
		return strings.Contains(msg, "ACL token not found") || strings.Contains(msg, "Permission denied")
	}
	return false
}

// watcher runs watchConfigs.
type watcher struct {
	events     EventsAPI
	mode       WatchMode
	logger     *slog.Logger
	metrics    Metrics
	controller string

	// retryBase and retryMax bound the backoff after errors.
	retryBase time.Duration
	retryMax  time.Duration
}

func (w *watcher) backoff(failures int) time.Duration {
	base, maxDelay := w.retryBase, w.retryMax
	if base <= 0 {
		base = time.Second
	}
	if maxDelay <= 0 {
		maxDelay = 30 * time.Second
	}
	d := base
	for i := 1; i < failures && d < maxDelay; i++ {
		d *= 2
	}
	return min(d, maxDelay)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// run keeps the subscription alive until ctx is canceled: it lists, streams
// the events after the listing with exact resumption, resumes after
// disconnections, and lists again when the server cannot resume (410 Gone).
//
// A listing index of 0 means that the listing does not cover the topics; the
// stream then starts at the newest events ("from now") and continues with
// exact resumption from the first event received. The same happens after a
// 410 Gone if cfg.fromNowAfterGone is set: sources whose listing index does
// not bound the indexes of all their topics would otherwise get 410 again.
func (w *watcher) run(ctx context.Context, cfg watchConfig) {
	logger := w.logger.With("watch", cfg.name)
	polling := w.mode == WatchPoll && cfg.poll != nil

	var (
		next     uint64 // 0: from now
		relist   = true
		reason   = "start"
		fromNow  bool
		failures int
		listedAt time.Time
	)
	for ctx.Err() == nil {
		if relist {
			index, err := cfg.list(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				failures++
				logger.Warn("listing failed", "error", err)
				sleepCtx(ctx, w.backoff(failures))
				continue
			}
			w.metrics.Resync(w.controller, cfg.name, reason)
			logger.Debug("listed", "index", index, "reason", reason)
			next, relist, failures, listedAt = index+1, false, 0, time.Now()
			if index == 0 || fromNow {
				next, fromNow = 0, false
			}
		}

		if cfg.resyncPeriod > 0 && time.Since(listedAt) >= cfg.resyncPeriod {
			relist, reason = true, "periodic"
			continue
		}

		if polling {
			index, err := cfg.poll(ctx, max(next, 1)-1)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				failures++
				logger.Warn("polling failed", "error", err)
				sleepCtx(ctx, w.backoff(failures))
				continue
			}
			failures = 0
			// A lower index means the server state was reset (e.g. a dev
			// agent restarted); the poll already synchronized from it.
			next = index + 1
			continue
		}

		var (
			streamCtx context.Context
			cancel    context.CancelFunc
		)
		if cfg.resyncPeriod > 0 {
			streamCtx, cancel = context.WithDeadline(ctx, listedAt.Add(cfg.resyncPeriod))
		} else {
			streamCtx, cancel = context.WithCancel(ctx)
		}
		started := next
		ch, err := w.events.Stream(streamCtx, cfg.namespace, cfg.topics, next, next > 0)
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return
			}
			switch {
			case errors.Is(err, api.ErrEventIndexGone):
				logger.Info("event index no longer available, resynchronizing", "index", next)
				relist, reason, fromNow = true, "gone", cfg.fromNowAfterGone
				continue
			case streamForbidden(err) && cfg.poll != nil && w.mode == WatchAuto:
				logger.Warn("event stream not permitted, falling back to polling with blocking queries", "error", err)
				polling = true
				continue
			}
			failures++
			logger.Warn("subscribing to the event stream failed", "error", err)
			sleepCtx(ctx, w.backoff(failures))
			continue
		}
		failures = 0
		logger.Debug("subscribed", "index", next)

	stream:
		for events := range ch {
			if events.Err != nil {
				if streamCtx.Err() == nil {
					logger.Debug("event stream interrupted", "error", events.Err)
				}
				break
			}
			for i := range events.Events {
				ev := &events.Events[i]
				w.metrics.EventReceived(w.controller, string(ev.Topic))
				if err := cfg.apply(ctx, ev); err != nil {
					if ctx.Err() == nil {
						logger.Warn("applying event failed, resynchronizing", "index", ev.Index, "error", err)
						relist, reason = true, "error"
					}
					break stream
				}
			}
			if events.Index >= next {
				next = events.Index + 1
			}
		}
		cancel()
		if started == 0 && next == 0 && !relist {
			// The stream started "from now" and ended before an event told
			// its position: changes until the next subscription could be
			// missed, so list again.
			relist, reason = true, "reconnect"
		}
		if ctx.Err() == nil && !relist && (cfg.resyncPeriod <= 0 || time.Since(listedAt) < cfg.resyncPeriod) {
			// Reconnect after a short pause; the exact index guarantees
			// that nothing is lost in between.
			sleepCtx(ctx, w.backoff(1))
		}
	}
}

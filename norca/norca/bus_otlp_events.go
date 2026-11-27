// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/bacanoo-io/norca/norca/helper/telemetry"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/nats-io/nats.go/jetstream"
)

// The export of the event stream as OTLP log records
// (telemetry.otlp.logs.events). The leader reads the event stream and the
// admin event stream of the region through a durable consumer of each, and
// acknowledges an event once the receiver accepted it. The consumers live in
// JetStream, replicated like their streams, so a new leader or a restarted
// server continues after the last acknowledged event: an event is exported
// at least once (an export whose acknowledgement was lost is repeated).
//
// No token is involved: the leader reads the streams through its own
// in-process connection, as it writes them, and the agent configuration
// that turns the export on and lists its topics is the authority. The
// topics that may carry secrets or identities are exported only when that
// configuration lists them.
//
// While the receiver is down the leader keeps the one batch it fetched,
// retries it with a backoff and fetches nothing more: the events wait in the
// stream (72 hours by default), not in memory.
//
// A leader with the export off deletes the consumers, so they neither count
// toward bus.events.max_consumers nor send the events of the pause when the
// export is turned on again; a leader that exports other topics than the
// consumer was made for deletes it too and starts with the new events.

// EventExporter is the export of the event stream: telemetry.EventExporter.
type EventExporter interface {
	// Topics are the exported topics.
	Topics() []string

	// Export sends the events and returns once the receiver accepted them.
	Export(ctx context.Context, events []telemetry.Event) error
}

const (
	// otlpEventsConsumer is the name of the durable consumers of the export.
	otlpEventsConsumer = "norca-otlp-export"

	// otlpEventsFetch is the most events one fetch takes.
	otlpEventsFetch = 100

	// otlpEventsFetchWait is how long a fetch waits for the first event.
	otlpEventsFetchWait = 5 * time.Second

	// otlpEventsAckWait is how long JetStream waits for the acknowledgement
	// of a fetched event before it delivers the event again: an event a
	// leader fetched and lost (it stopped without returning it) is late by
	// up to this. The export marks the events it holds as in progress before
	// every attempt, and an export attempt lasts at most telemetry.otlp's
	// timeout.
	otlpEventsAckWait = time.Minute
)

// otlpEventStream is a stream the export reads, with the subjects of the
// exported topics in it.
type otlpEventStream struct {
	name    string
	filters []string
}

// otlpEventStreams are the event stream and the admin event stream of the
// region, filtered to the exported topics. A topic is looked for in both:
// the network policies are in either, by their kind.
func (s *Server) otlpEventStreams(topics []string) []otlpEventStream {
	prefix := s.busEventsPrefix()
	region := bus.SubjectToken(s.config.Region)
	events := otlpEventStream{name: bus.EventStreamName(s.config.Region)}
	admin := otlpEventStream{name: bus.AdminEventStreamName(s.config.Region)}
	for _, topic := range topics {
		t := bus.SubjectToken(topic)
		events.filters = append(events.filters, prefix+"."+region+".*."+t)
		admin.filters = append(admin.filters, prefix+"."+bus.AdminSubjectToken+"."+region+".*."+t)
	}
	return []otlpEventStream{events, admin}
}

// exportBusEventsOTLP exports the events of one stream with exp until ctx
// ends (the leadership is lost) or the bus fails.
func (s *Server) exportBusEventsOTLP(ctx context.Context, js jetstream.JetStream, st otlpEventStream, exp EventExporter) error {
	if err := s.dropChangedOTLPConsumer(ctx, js, st); err != nil {
		return err
	}
	cons, err := js.CreateOrUpdateConsumer(ctx, st.name, jetstream.ConsumerConfig{
		Durable:        otlpEventsConsumer,
		Description:    "Norca: the export of the events as OTLP log records",
		FilterSubjects: st.filters,
		AckPolicy:      jetstream.AckExplicitPolicy,
		AckWait:        otlpEventsAckWait,
		// The first leader with the export starts with the new events: the
		// events kept from before are not sent in one burst.
		DeliverPolicy: jetstream.DeliverNewPolicy,
	})
	if err != nil {
		return fmt.Errorf("the consumer of %s: %w", st.name, err)
	}
	s.logger.Debug("exporting the events over OTLP", "stream", st.name, "subjects", st.filters)
	for ctx.Err() == nil {
		msgs, err := fetchOTLPEvents(ctx, cons)
		if ctx.Err() != nil {
			nakAll(msgs)
			return nil
		}
		if err != nil {
			return fmt.Errorf("fetching the events of %s: %w", st.name, err)
		}
		s.exportOTLPEvents(ctx, exp, msgs)
	}
	return nil
}

// dropChangedOTLPConsumer deletes the consumer of the export of a stream
// when it was made for other topics. Updated in place it would go on from
// its position and deliver the events of an added topic since then in one
// burst; the new consumer starts with the new events, as a first export
// does (the events of the other topics not exported yet are skipped too).
func (s *Server) dropChangedOTLPConsumer(ctx context.Context, js jetstream.JetStream, st otlpEventStream) error {
	cons, err := js.Consumer(ctx, st.name, otlpEventsConsumer)
	if errors.Is(err, jetstream.ErrConsumerNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("the consumer of %s: %w", st.name, err)
	}
	cfg := cons.CachedInfo().Config
	filters := cfg.FilterSubjects
	if len(filters) == 0 && cfg.FilterSubject != "" {
		filters = []string{cfg.FilterSubject}
	}
	if sameStrings(filters, st.filters) {
		return nil
	}
	err = js.DeleteConsumer(ctx, st.name, otlpEventsConsumer)
	if err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
		return fmt.Errorf("deleting the consumer of %s: %w", st.name, err)
	}
	s.logger.Info("the exported event topics changed: the OTLP export starts with the new events",
		"stream", st.name, "subjects", st.filters)
	return nil
}

// removeOTLPEventConsumers deletes the consumers of the export from both
// event streams of the region, on a leader with the export off, and waits
// for the end of the leadership, as the other bus loops do.
func (s *Server) removeOTLPEventConsumers(ctx context.Context, js jetstream.JetStream) error {
	for _, name := range []string{bus.EventStreamName(s.config.Region), bus.AdminEventStreamName(s.config.Region)} {
		err := js.DeleteConsumer(ctx, name, otlpEventsConsumer)
		switch {
		case err == nil:
			s.logger.Info("removed the consumer of the OTLP event export, which is off", "stream", name)
		case errors.Is(err, jetstream.ErrConsumerNotFound), errors.Is(err, jetstream.ErrStreamNotFound):
		default:
			return fmt.Errorf("removing the consumer of %s: %w", name, err)
		}
	}
	<-ctx.Done()
	return nil
}

// sameStrings reports whether a and b hold the same strings, in any order.
func sameStrings(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// fetchOTLPEvents waits for the next event, up to otlpEventsFetchWait, and
// takes the ones after it that are there already, up to otlpEventsFetch in
// all. It returns no events when none came.
func fetchOTLPEvents(ctx context.Context, cons jetstream.Consumer) ([]jetstream.Msg, error) {
	wait, cancel := context.WithTimeout(ctx, otlpEventsFetchWait)
	defer cancel()
	first, err := cons.Fetch(1, jetstream.FetchContext(wait))
	if err != nil {
		return nil, err
	}
	msgs := collectMsgs(first)
	if len(msgs) == 0 {
		if err := first.Error(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, nil
	}
	rest, err := cons.FetchNoWait(otlpEventsFetch - 1)
	if err != nil {
		// the first one goes out alone
		return msgs, nil //nolint:nilerr // the events after it come with the next fetch
	}
	return append(msgs, collectMsgs(rest)...), nil
}

func collectMsgs(batch jetstream.MessageBatch) []jetstream.Msg {
	var msgs []jetstream.Msg
	for msg := range batch.Messages() {
		msgs = append(msgs, msg)
	}
	return msgs
}

// nakAll hands fetched events back to JetStream, which delivers them again
// at once: to the next leader when the leadership ends.
func nakAll(msgs []jetstream.Msg) {
	for _, msg := range msgs {
		_ = msg.Nak()
	}
}

// exportOTLPEvents exports fetched events, in chunks of at most
// telemetry.MaxEventBatchBytes, and acknowledges each chunk once it is out.
// It returns when all are out, or when ctx ended, handing the rest back.
func (s *Server) exportOTLPEvents(ctx context.Context, exp EventExporter, msgs []jetstream.Msg) {
	for len(msgs) > 0 {
		n := otlpEventChunk(msgs)
		chunk := msgs[:n]
		events := make([]telemetry.Event, 0, n)
		for _, msg := range chunk {
			events = append(events, otlpEvent(msg))
		}
		if !s.exportOTLPEventChunk(ctx, exp, events, msgs) {
			nakAll(msgs)
			return
		}
		for _, msg := range chunk {
			if err := msg.Ack(); err != nil {
				// JetStream delivers it again: exported twice
				s.logger.Debug("acknowledging an exported event failed", "error", err)
			}
		}
		msgs = msgs[n:]
	}
}

// exportOTLPEventChunk exports events until the receiver accepts them, with
// a backoff between the attempts, and reports false when ctx ended first.
// The fetched events that are not acknowledged yet (held) are marked in
// progress before every attempt, so JetStream does not deliver them again
// meanwhile. The exporter reports the failures (a rate-limited warning and
// the norca.agent.otlp.export_errors counter).
func (s *Server) exportOTLPEventChunk(ctx context.Context, exp EventExporter, events []telemetry.Event, held []jetstream.Msg) bool {
	for failures := 0; ; failures++ {
		if failures > 0 {
			wait := min(busRetryMin<<min(failures-1, 5), busRetryMax)
			select {
			case <-ctx.Done():
				return false
			case <-time.After(wait):
			}
			for _, msg := range held {
				_ = msg.InProgress()
			}
		}
		err := exp.Export(ctx, events)
		if err == nil {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		s.logger.Debug("exporting events over OTLP failed, retrying", "error", err, "events", len(events))
	}
}

// otlpEventChunk returns how many of msgs, at least one, fit into one
// export by the size of their payloads.
func otlpEventChunk(msgs []jetstream.Msg) int {
	size := 0
	for i, msg := range msgs {
		size += min(len(msg.Data()), telemetry.MaxEventBody)
		if size > telemetry.MaxEventBatchBytes && i > 0 {
			return i
		}
	}
	return len(msgs)
}

// otlpEvent converts a message of an event stream: the metadata from its
// headers, the payload from its JSON, the time it was stored.
func otlpEvent(msg jetstream.Msg) telemetry.Event {
	h := msg.Headers()
	index, _ := strconv.ParseUint(h.Get(bus.HeaderIndex), 10, 64)
	ev := telemetry.Event{
		Topic:     h.Get(bus.HeaderTopic),
		Type:      h.Get(bus.HeaderType),
		Key:       h.Get(bus.HeaderKey),
		Namespace: h.Get(bus.HeaderNamespace),
		Index:     index,
		Payload:   eventPayload(msg.Data()),
	}
	if md, err := msg.Metadata(); err == nil {
		ev.Time = md.Timestamp
	}
	return ev
}

// eventPayload returns the Payload of the JSON of an event message, or the
// whole JSON when it has none.
func eventPayload(data []byte) []byte {
	var ev struct{ Payload json.RawMessage }
	if err := json.Unmarshal(data, &ev); err == nil && len(ev.Payload) > 0 {
		return ev.Payload
	}
	return data
}

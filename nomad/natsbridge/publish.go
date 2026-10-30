// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package natsbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Headers of the published messages. Nats-Msg-Id is the JetStream
// deduplication header; the others repeat the metadata of the event so that
// consumers can route and filter without decoding the payload.
const (
	HeaderMsgID     = "Nats-Msg-Id"
	HeaderRegion    = "Nomad-Region"
	HeaderIndex     = "Nomad-Index"
	HeaderTopic     = "Nomad-Topic"
	HeaderType      = "Nomad-Type"
	HeaderNamespace = "Nomad-Namespace"
	HeaderKey       = "Nomad-Key"
)

// Gap marker published when events may have been lost.
const (
	// GapTopic is the topic (last subject token) of gap markers.
	GapTopic = "Bridge"

	// GapType is the event type of gap markers.
	GapType = "EventGap"
)

// GapPayload is the payload of a gap marker: the events of the region with
// an index of FromIndex or higher, up to the next event the bridge
// publishes for the region, may be missing.
type GapPayload struct {
	FromIndex uint64
	Reason    string
	Time      time.Time
}

// validToken checks that s can be used as one token of a NATS subject.
func validToken(s string) error {
	if s == "" {
		return errors.New("empty")
	}
	if strings.ContainsAny(s, ". *>\t\r\n") {
		return errors.New(`must not contain ".", "*", ">" or white space`)
	}
	return nil
}

// validPrefix checks a subject prefix: one or more tokens separated by dots.
func validPrefix(s string) error {
	for _, tok := range strings.Split(s, ".") {
		if err := validToken(tok); err != nil {
			return fmt.Errorf("invalid subject prefix %q: %w", s, err)
		}
	}
	return nil
}

// subjectToken replaces the characters a subject token cannot contain.
func subjectToken(s string) string {
	if s == "" {
		return "_"
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '.', '*', '>', ' ', '\t', '\r', '\n':
			return '_'
		}
		return r
	}, s)
}

// Subject returns the subject of the events of a topic.
func Subject(prefix, region, topic string) string {
	return prefix + "." + subjectToken(region) + "." + subjectToken(topic)
}

// MsgID returns the deduplication ID of an event: the same event of a
// region gets the same ID however often it is delivered.
func MsgID(region string, ev *Event) string {
	return fmt.Sprintf("%s:%d:%s:%s:%s:%s", region, ev.Index, ev.Topic, ev.Type, ev.Namespace, ev.Key)
}

// withRegion adds a "Region" field to an event JSON object, keeping the
// rest of it byte for byte.
func withRegion(raw []byte, region string) []byte {
	r, _ := json.Marshal(region)
	out := make([]byte, 0, len(raw)+len(r)+12)
	out = append(out, `{"Region":`...)
	out = append(out, r...)
	rest := raw[1:] // after "{"
	if trimmed := strings.TrimSpace(string(rest)); trimmed != "}" {
		out = append(out, ',')
	}
	return append(out, rest...)
}

// eventMessage builds the message of an event.
func eventMessage(prefix, region string, ev *Event) *nats.Msg {
	data := []byte(ev.Raw)
	if len(data) > 0 && data[0] == '{' {
		data = withRegion(data, region)
	}
	msg := nats.NewMsg(Subject(prefix, region, ev.Topic))
	msg.Data = data
	msg.Header.Set(HeaderMsgID, MsgID(region, ev))
	msg.Header.Set(HeaderRegion, region)
	msg.Header.Set(HeaderIndex, strconv.FormatUint(ev.Index, 10))
	msg.Header.Set(HeaderTopic, ev.Topic)
	msg.Header.Set(HeaderType, ev.Type)
	if ev.Namespace != "" {
		msg.Header.Set(HeaderNamespace, ev.Namespace)
	}
	if ev.Key != "" {
		msg.Header.Set(HeaderKey, ev.Key)
	}
	return msg
}

// gapMessage builds a gap marker. Its JSON has the shape of an event.
func gapMessage(prefix, region string, fromIndex uint64, reason string, now time.Time) *nats.Msg {
	ev := struct {
		Region    string
		Topic     string
		Type      string
		Key       string
		Namespace string
		Index     uint64
		Payload   map[string]GapPayload
	}{
		Region:  region,
		Topic:   GapTopic,
		Type:    GapType,
		Key:     region,
		Payload: map[string]GapPayload{"Gap": {FromIndex: fromIndex, Reason: reason, Time: now.UTC()}},
	}
	data, _ := json.Marshal(ev)
	msg := nats.NewMsg(Subject(prefix, region, GapTopic))
	msg.Data = data
	// A gap marker published again for the same position (e.g. after a
	// restart before the next event) is a duplicate.
	msg.Header.Set(HeaderMsgID, fmt.Sprintf("%s:gap:%d", region, fromIndex))
	msg.Header.Set(HeaderRegion, region)
	msg.Header.Set(HeaderTopic, GapTopic)
	msg.Header.Set(HeaderType, GapType)
	msg.Header.Set(HeaderKey, region)
	return msg
}

// Publisher publishes messages to NATS.
type Publisher interface {
	// PublishBatch publishes msgs in order and returns once all of them
	// are acknowledged by JetStream or, for core NATS, flushed to the
	// server. On an error any prefix of msgs may have been published; the
	// caller publishes the whole batch again.
	PublishBatch(ctx context.Context, msgs []*nats.Msg) (duplicates int, err error)
}

// CorePublisher publishes with core NATS: a batch is followed by a flush,
// which returns once the server has received the messages.
type CorePublisher struct {
	Conn    *nats.Conn
	Timeout time.Duration
}

func (p *CorePublisher) PublishBatch(ctx context.Context, msgs []*nats.Msg) (int, error) {
	if len(msgs) == 0 {
		return 0, nil
	}
	for _, msg := range msgs {
		if err := p.Conn.PublishMsg(msg); err != nil {
			return 0, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeoutOr(p.Timeout))
	defer cancel()
	return 0, p.Conn.FlushWithContext(ctx)
}

// JetStreamPublisher publishes with JetStream and waits for the
// acknowledgement of every message. The stream deduplicates messages by
// their Nats-Msg-Id header within its duplicate window.
type JetStreamPublisher struct {
	JS      jetstream.JetStream
	Timeout time.Duration

	// Stream, if set, is the stream the messages must be stored in.
	Stream string
}

func (p *JetStreamPublisher) PublishBatch(ctx context.Context, msgs []*nats.Msg) (int, error) {
	if len(msgs) == 0 {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeoutOr(p.Timeout))
	defer cancel()

	var opts []jetstream.PublishOpt
	if p.Stream != "" {
		opts = append(opts, jetstream.WithExpectStream(p.Stream))
	}
	futures := make([]jetstream.PubAckFuture, 0, len(msgs))
	for _, msg := range msgs {
		f, err := p.JS.PublishMsgAsync(msg, opts...)
		if err != nil {
			return 0, err
		}
		futures = append(futures, f)
	}
	duplicates := 0
	for _, f := range futures {
		select {
		case ack := <-f.Ok():
			if ack.Duplicate {
				duplicates++
			}
		case err := <-f.Err():
			return 0, err
		case <-ctx.Done():
			return 0, fmt.Errorf("waiting for the JetStream acknowledgement: %w", ctx.Err())
		}
	}
	return duplicates, nil
}

func timeoutOr(d time.Duration) time.Duration {
	if d <= 0 {
		return 10 * time.Second
	}
	return d
}

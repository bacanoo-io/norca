// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package bus

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

// The messages of the event stream: one per event of the event broker, on
// <prefix>.<region>.<topic>, with the event JSON of the event stream API and
// the metadata in headers.

// Headers of the event messages. Nats-Msg-Id is the JetStream deduplication
// header; the others repeat the metadata of the event so that consumers can
// filter without decoding the payload.
const (
	HeaderMsgID     = "Nats-Msg-Id"
	HeaderRegion    = "Nomad-Region"
	HeaderIndex     = "Nomad-Index"
	HeaderTopic     = "Nomad-Topic"
	HeaderType      = "Nomad-Type"
	HeaderNamespace = "Nomad-Namespace"
	HeaderKey       = "Nomad-Key"
)

const (
	// GapTopic and GapType mark a gap: events may be missing from the
	// stream from GapPayload.FromIndex up to the next event.
	GapTopic = "Bus"
	GapType  = "EventGap"

	// PushSubject is the subject (after the prefix) of pack push notices.
	PushSubject = "packs.pushed"

	// DefaultSubjectPrefix is the first part of the event subjects.
	DefaultSubjectPrefix = "nomad"
)

// GapPayload is the payload of a gap marker.
type GapPayload struct {
	FromIndex uint64
	Reason    string
	Time      time.Time
}

// Event is an event of the broker to publish: its metadata and its JSON as
// the event stream API encodes it.
type Event struct {
	Index     uint64
	Topic     string
	Type      string
	Namespace string
	Key       string
	JSON      []byte
}

// SubjectToken replaces the characters a subject token can't contain.
func SubjectToken(s string) string {
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

// ValidSubjectPrefix checks a subject prefix: tokens separated by dots.
func ValidSubjectPrefix(prefix string) error {
	for _, tok := range strings.Split(prefix, ".") {
		if tok == "" || strings.ContainsAny(tok, " *>\t\r\n") {
			return fmt.Errorf("invalid subject prefix %q", prefix)
		}
	}
	return nil
}

// EventSubject is the subject of the events of a topic of a region.
func EventSubject(prefix, region, topic string) string {
	return prefix + "." + SubjectToken(region) + "." + SubjectToken(topic)
}

// EventStreamName is the name of the event stream of a region.
func EventStreamName(region string) string {
	return "NORCA_EVENTS_" + strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		}
		return '_'
	}, region)
}

// EventMsgID is the deduplication ID of an event: the same event of a
// region gets the same ID however often it is published.
func EventMsgID(region string, ev *Event) string {
	return fmt.Sprintf("%s:%d:%s:%s:%s:%s", region, ev.Index, ev.Topic, ev.Type, ev.Namespace, ev.Key)
}

// EventMessage builds the message of an event: its JSON with a "Region"
// field in front, and the metadata headers.
func EventMessage(prefix, region string, ev *Event) *nats.Msg {
	data := ev.JSON
	if len(data) > 0 && data[0] == '{' {
		data = withRegion(data, region)
	}
	msg := nats.NewMsg(EventSubject(prefix, region, ev.Topic))
	msg.Data = data
	msg.Header.Set(HeaderMsgID, EventMsgID(region, ev))
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

// withRegion adds a "Region" field to a JSON object, keeping the rest of it
// byte for byte.
func withRegion(raw []byte, region string) []byte {
	r, _ := json.Marshal(region)
	out := make([]byte, 0, len(raw)+len(r)+12)
	out = append(out, `{"Region":`...)
	out = append(out, r...)
	rest := raw[1:]
	if strings.TrimSpace(string(rest)) != "}" {
		out = append(out, ',')
	}
	return append(out, rest...)
}

// GapMessage builds a gap marker; its JSON has the shape of an event.
func GapMessage(prefix, region string, fromIndex uint64, reason string, now time.Time) *nats.Msg {
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
	msg := nats.NewMsg(EventSubject(prefix, region, GapTopic))
	msg.Data = data
	msg.Header.Set(HeaderMsgID, fmt.Sprintf("%s:gap:%d", region, fromIndex))
	msg.Header.Set(HeaderRegion, region)
	msg.Header.Set(HeaderTopic, GapTopic)
	msg.Header.Set(HeaderType, GapType)
	msg.Header.Set(HeaderKey, region)
	return msg
}

// LastEventIndex returns the Raft index of the newest event in a stream, by
// the Nomad-Index header of its last messages (gap markers have none).
// found is false for an empty stream or one without events.
func LastEventIndex(ctx context.Context, stream jetstream.Stream) (index uint64, found bool, err error) {
	info, err := stream.Info(ctx)
	if err != nil {
		return 0, false, err
	}
	const lookBack = 64
	for seq := info.State.LastSeq; seq >= info.State.FirstSeq && seq > 0 && info.State.LastSeq-seq < lookBack; seq-- {
		msg, err := stream.GetMsg(ctx, seq)
		if errors.Is(err, jetstream.ErrMsgNotFound) {
			continue
		}
		if err != nil {
			return 0, false, err
		}
		if v := msg.Header.Get(HeaderIndex); v != "" {
			idx, err := strconv.ParseUint(v, 10, 64)
			if err == nil {
				return idx, true, nil
			}
		}
	}
	return 0, false, nil
}

// StreamSettings are the limits of a stream the server manages.
type StreamSettings struct {
	Replicas    int
	MaxAge      time.Duration
	MaxBytes    int64
	MaxMsgSize  int32
	Duplicates  time.Duration
	Storage     jetstream.StorageType
	Compression jetstream.StoreCompression
}

// EnsureStream creates a stream or updates the settings of an existing one
// that differ. The storage type of an existing stream can't change and is
// kept.
func EnsureStream(ctx context.Context, js jetstream.JetStream, name, description string, subjects []string, set StreamSettings) error {
	apply := func(cfg *jetstream.StreamConfig) {
		cfg.Replicas = max(set.Replicas, 1)
		cfg.MaxAge = set.MaxAge
		cfg.MaxBytes = set.MaxBytes
		if cfg.MaxBytes <= 0 {
			cfg.MaxBytes = -1
		}
		cfg.MaxMsgSize = set.MaxMsgSize
		if cfg.MaxMsgSize <= 0 {
			cfg.MaxMsgSize = -1
		}
		cfg.Duplicates = set.Duplicates
		cfg.Discard = jetstream.DiscardOld
		if cfg.Storage == jetstream.FileStorage {
			cfg.Compression = set.Compression
		}
	}

	stream, err := js.Stream(ctx, name)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		cfg := jetstream.StreamConfig{
			Name:        name,
			Description: description,
			Subjects:    subjects,
			Storage:     set.Storage,
		}
		apply(&cfg)
		_, err = js.CreateStream(ctx, cfg)
		if errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
			return nil
		}
		return err
	}
	if err != nil {
		return err
	}
	cfg := stream.CachedInfo().Config
	before := cfg
	apply(&cfg)
	if cfg.Replicas == before.Replicas && cfg.MaxAge == before.MaxAge && cfg.MaxBytes == before.MaxBytes &&
		cfg.MaxMsgSize == before.MaxMsgSize && cfg.Duplicates == before.Duplicates &&
		cfg.Discard == before.Discard && cfg.Compression == before.Compression {
		return nil
	}
	_, err = js.UpdateStream(ctx, cfg)
	return err
}

// PushNotice is the message of a pack push: the registry source (or the
// name of a cluster registry) that got new packs.
type PushNotice struct {
	Source   string `json:"Source,omitempty"`
	Registry string `json:"Registry,omitempty"`

	// Pack, Version and Digest are informational.
	Pack    string `json:"Pack,omitempty"`
	Version string `json:"Version,omitempty"`
	Digest  string `json:"Digest,omitempty"`
}

// NormalizeSource makes registry sources comparable: lower-case scheme, no
// trailing slash or ".git".
func NormalizeSource(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i > 0 {
		s = strings.ToLower(s[:i]) + s[i:]
	}
	s = strings.TrimSuffix(s, "/")
	return strings.TrimSuffix(s, ".git")
}

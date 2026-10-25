// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package natsbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/nomad/api"
)

// ErrIndexGone is returned by EventSource.Stream when an exact subscription
// cannot be served because events at or after the requested index were
// dropped from the event buffer of the server (410 Gone).
var ErrIndexGone = errors.New("event index no longer available")

// Event is one event of the event stream. Raw is the event JSON exactly as
// the server sent it; the other fields are decoded from it.
type Event struct {
	Topic     string
	Type      string
	Key       string
	Namespace string
	Index     uint64
	Raw       json.RawMessage
}

// Batch is one line of the event stream: the events of one Raft index.
type Batch struct {
	Index  uint64
	Events []Event

	// Err is set on the last value sent before the channel is closed when
	// the stream ended because of an error.
	Err error
}

// EventSource is a subscription to the event stream.
type EventSource interface {
	// Stream subscribes to the event stream. With exact set, every event
	// with an index at or after index is delivered, or the call fails with
	// an error wrapping ErrIndexGone; index must be positive then. Without
	// exact, index 0 starts at the newest events, a positive index at the
	// oldest buffered events at or after it.
	//
	// The channel is closed when the stream ends or ctx is canceled.
	Stream(ctx context.Context, index uint64, exact bool) (<-chan Batch, error)
}

// TopicFilter selects events of a topic, optionally only those with one of
// the given keys (server-side filtering). Keys of the Variables topic may be
// glob patterns.
type TopicFilter struct {
	Topic string
	Keys  []string
}

// ParseTopics parses a comma separated list of topic filters. Each element
// is "Topic" or "Topic:key"; repeated topics collect their keys, for example
// "Job:web,Job:api,Node".
func ParseTopics(s string) ([]TopicFilter, error) {
	byTopic := map[string][]string{}
	var order []string
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		topic, key, hasKey := strings.Cut(item, ":")
		if topic == "" || (hasKey && key == "") {
			return nil, fmt.Errorf("invalid topic filter %q", item)
		}
		if err := validToken(topic); err != nil {
			return nil, fmt.Errorf("invalid topic %q: %w", topic, err)
		}
		if _, seen := byTopic[topic]; !seen {
			order = append(order, topic)
			byTopic[topic] = nil
		}
		if hasKey {
			byTopic[topic] = append(byTopic[topic], key)
		}
	}
	if len(order) == 0 {
		return nil, errors.New("no topics")
	}
	filters := make([]TopicFilter, 0, len(order))
	for _, topic := range order {
		filters = append(filters, TopicFilter{Topic: topic, Keys: byTopic[topic]})
	}
	return filters, nil
}

// APISource reads the event stream through the HTTP API.
//
// It does not use api.EventStream: that decodes the payloads into generic
// maps, which turns large integers (indexes, nanosecond timestamps) into
// floats. The bridge forwards the event JSON unchanged instead.
type APISource struct {
	client    *api.Client
	region    string
	namespace string
	query     string
}

// NewAPISource returns an event source for the region and namespace ("*"
// for all namespaces) of the client.
func NewAPISource(client *api.Client, region, namespace string, topics []TopicFilter) *APISource {
	params := url.Values{}
	for _, f := range topics {
		keys := f.Keys
		if len(keys) == 0 {
			keys = []string{"*"}
		}
		for _, k := range keys {
			params.Add("topic", f.Topic+":"+k)
		}
	}
	return &APISource{client: client, region: region, namespace: namespace, query: params.Encode()}
}

func (s *APISource) Stream(ctx context.Context, index uint64, exact bool) (<-chan Batch, error) {
	if exact && index == 0 {
		return nil, errors.New("an exact subscription requires a positive index")
	}
	params := map[string]string{"index": strconv.FormatUint(index, 10)}
	if exact {
		params["exact"] = "true"
	}
	q := (&api.QueryOptions{Region: s.region, Namespace: s.namespace, Params: params}).WithContext(ctx)
	body, err := s.client.Raw().Response("/v1/event/stream?"+s.query, q)
	if err != nil {
		var respErr api.UnexpectedResponseError
		if errors.As(err, &respErr) && respErr.StatusCode() == http.StatusGone {
			return nil, fmt.Errorf("%w: %v", ErrIndexGone, err)
		}
		return nil, err
	}

	ch := make(chan Batch, 16)
	go func() {
		defer close(ch)
		defer body.Close()
		readStream(ctx, body, ch)
	}()
	return ch, nil
}

// readStream decodes newline delimited event batches from r until it ends
// or ctx is canceled.
func readStream(ctx context.Context, r io.Reader, ch chan<- Batch) {
	dec := json.NewDecoder(r)
	for ctx.Err() == nil {
		var line struct {
			Index  uint64
			Events []json.RawMessage
		}
		var batch Batch
		if err := dec.Decode(&line); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			batch.Err = err
		} else if len(line.Events) == 0 {
			// Heartbeat.
			continue
		} else {
			batch, err = decodeBatch(line.Index, line.Events)
			if err != nil {
				batch = Batch{Err: err}
			}
		}
		select {
		case <-ctx.Done():
			return
		case ch <- batch:
		}
		if batch.Err != nil {
			return
		}
	}
}

func decodeBatch(index uint64, raws []json.RawMessage) (Batch, error) {
	batch := Batch{Index: index, Events: make([]Event, 0, len(raws))}
	for _, raw := range raws {
		var ev Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			return Batch{}, fmt.Errorf("decoding an event of index %d: %w", index, err)
		}
		ev.Raw = bytes.TrimSpace(raw)
		if ev.Index == 0 {
			ev.Index = index
		}
		batch.Events = append(batch.Events, ev)
	}
	return batch, nil
}

// topicsString formats topic filters for logs.
func topicsString(filters []TopicFilter) string {
	parts := make([]string, 0, len(filters))
	for _, f := range filters {
		if len(f.Keys) == 0 {
			parts = append(parts, f.Topic)
			continue
		}
		keys := append([]string(nil), f.Keys...)
		sort.Strings(keys)
		for _, k := range keys {
			parts = append(parts, f.Topic+":"+k)
		}
	}
	return strings.Join(parts, ",")
}

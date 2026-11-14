// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"net/url"
	"time"
)

// The change streams of a region (change data capture).
const (
	CDCStreamEvents = "events"
	CDCStreamAdmin  = "admin"
	CDCStreamKV     = "kv"
)

// CDCConsumerSpec is what a reader asks for its consumer of a change stream:
// the filters within its cdc rule, where a new consumer starts (all, new,
// last-per-subject, by-start-time with StartTime), the acknowledgement wait
// and the bound of unacknowledged events.
type CDCConsumerSpec struct {
	Filters       []string
	Deliver       string    `json:",omitempty"`
	StartTime     time.Time `json:",omitempty"`
	AckWait       time.Duration
	MaxAckPending int
}

// CDCConsumer is the consumer the servers keep for a reader of a change
// stream. The reader reads it on the system bus by its Name (the accessor ID
// of its token, or <allocation ID>_<task> of a workload identity) in the
// stream StreamName.
type CDCConsumer struct {
	Stream        string
	StreamName    string
	Name          string
	Filters       []string
	Deliver       string
	StartTime     time.Time
	AckWait       time.Duration
	MaxAckPending int
	NumPending    uint64
	NumAckPending int
	Delivered     uint64
	Created       time.Time
}

// CDC is the change data capture API: the consumers the servers keep for
// the readers of the change streams.
type CDC struct {
	client *Client
}

// CDC returns a handle on the change data capture API.
func (c *Client) CDC() *CDC {
	return &CDC{client: c}
}

func cdcConsumerPath(stream string) string {
	return "/v1/cdc/consumer/" + url.PathEscape(stream)
}

// UpsertConsumer creates or changes the reader's consumer of a stream; a
// change keeps its position.
func (c *CDC) UpsertConsumer(stream string, spec *CDCConsumerSpec, q *WriteOptions) (*CDCConsumer, *WriteMeta, error) {
	var out CDCConsumer
	wm, err := c.client.put(cdcConsumerPath(stream), spec, &out, q)
	if err != nil {
		return nil, wm, err
	}
	return &out, wm, nil
}

// ReadConsumer reads the reader's consumer of a stream.
func (c *CDC) ReadConsumer(stream string, q *QueryOptions) (*CDCConsumer, *QueryMeta, error) {
	var out CDCConsumer
	qm, err := c.client.query(cdcConsumerPath(stream), &out, q)
	if err != nil {
		return nil, qm, err
	}
	return &out, qm, nil
}

// DeleteConsumer deletes the reader's consumer of a stream.
func (c *CDC) DeleteConsumer(stream string, q *WriteOptions) (*WriteMeta, error) {
	return c.client.delete(cdcConsumerPath(stream), nil, nil, q)
}

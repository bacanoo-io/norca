// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import "time"

// Change data capture (CDC): the consumers the servers keep for the readers
// of the change streams of the system bus, one per reader and stream, named
// after the reader and filtered within its cdc rule.
const (
	CDCConsumerUpsertRPCMethod = "CDC.ConsumerUpsert"
	CDCConsumerReadRPCMethod   = "CDC.ConsumerRead"
	CDCConsumerDeleteRPCMethod = "CDC.ConsumerDelete"

	// The change streams of a region, as the API names them.
	CDCStreamEvents = "events"
	CDCStreamAdmin  = "admin"
	CDCStreamKV     = "kv"

	// Where a new consumer starts.
	CDCDeliverAll            = "all"
	CDCDeliverNew            = "new"
	CDCDeliverLastPerSubject = "last-per-subject"
	CDCDeliverByStartTime    = "by-start-time"

	// CDCMaxFilters bounds the filters of a consumer.
	CDCMaxFilters = 16
)

// CDCConsumerSpec is what a reader asks for its consumer of a stream.
type CDCConsumerSpec struct {
	// Filters are the subjects of the stream the consumer delivers; each
	// lies within the reader's cdc rule, and they do not overlap.
	Filters []string

	// Deliver is where a new consumer starts: all (default), new,
	// last-per-subject or by-start-time (with StartTime). It can't change
	// for an existing consumer.
	Deliver   string
	StartTime time.Time

	// AckWait is how long a delivered event waits for its acknowledgement
	// before it is delivered again (default 30s). MaxAckPending bounds the
	// events delivered and not acknowledged, within the limit of the stream.
	AckWait       time.Duration
	MaxAckPending int
}

// CDCConsumer is a consumer the servers keep for a reader.
type CDCConsumer struct {
	// Stream is the change stream as the API names it (events, admin, kv);
	// StreamName its name on the bus.
	Stream     string
	StreamName string

	// Name is the name of the consumer: the accessor ID of the reader's
	// token, or <allocation ID>_<task> of a workload identity.
	Name string

	Filters       []string
	Deliver       string
	StartTime     time.Time `json:",omitempty"`
	AckWait       time.Duration
	MaxAckPending int

	// NumPending are the events of its filters not delivered yet,
	// NumAckPending those delivered and not acknowledged, Delivered the
	// stream sequence of the last delivered event.
	NumPending    uint64
	NumAckPending int
	Delivered     uint64
	Created       time.Time
}

// CDCConsumerUpsertRequest creates or changes the reader's consumer of a
// stream.
type CDCConsumerUpsertRequest struct {
	Stream   string
	Consumer CDCConsumerSpec
	WriteRequest
}

// CDCConsumerReadRequest reads the reader's consumer of a stream.
type CDCConsumerReadRequest struct {
	Stream string
	QueryOptions
}

// CDCConsumerDeleteRequest deletes the reader's consumer of a stream.
type CDCConsumerDeleteRequest struct {
	Stream string
	WriteRequest
}

// CDCConsumerResponse is the answer of a read.
type CDCConsumerResponse struct {
	Consumer *CDCConsumer
	QueryMeta
}

// CDCConsumerWriteResponse is the answer of an upsert or a delete.
type CDCConsumerWriteResponse struct {
	Consumer *CDCConsumer
	WriteMeta
}

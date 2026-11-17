// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// cdcConsumerTimeout bounds the JetStream requests of the endpoint.
	cdcConsumerTimeout = 10 * time.Second

	// cdcDefaultAckWait is the acknowledgement wait of a consumer that does
	// not name one.
	cdcDefaultAckWait = 30 * time.Second

	// The request limits of a consumer: one pull can't ask for everything.
	cdcMaxRequestBatch    = 256
	cdcMaxRequestMaxBytes = 1 << 20

	// cdcReaderMeta is the metadata key of a consumer that names its reader
	// (<kind>:<name>), for the cleanup of consumers whose reader lost its
	// right.
	cdcReaderMeta = "norca.reader"
)

// CDC is the endpoint of the consumers the servers keep for the readers of
// the change streams (change data capture): one per reader and stream, named
// after the reader, its filters within the reader's cdc rule. A reader reads
// its consumer through the bus with its token (info, next and acks of its
// own consumer, see busGrantSubjects); it never creates or changes one
// itself.
type CDC struct {
	srv *Server
	ctx *RPCContext
}

func NewCDCEndpoint(srv *Server, ctx *RPCContext) *CDC {
	return &CDC{srv: srv, ctx: ctx}
}

// cdcReader is the reader of a request.
type cdcReader struct {
	kind     string
	name     string
	consumer string
	grant    *acl.CDCGrant
}

// cdcRequestReader resolves the reader of a request: its token or workload
// identity, with a cdc rule.
func (s *Server) cdcRequestReader(args structs.RequestWithIdentity) (*cdcReader, error) {
	if !s.config.ACLEnabled {
		return nil, structs.NewErrRPCCoded(http.StatusBadRequest,
			"change data capture consumers need ACLs: a consumer is named after the token or workload identity of its reader")
	}
	aclObj, err := s.ResolveACL(args)
	if err != nil {
		return nil, err
	}
	reader := &cdcReader{grant: aclObj.CDCGrant()}
	identity := args.GetIdentity()
	switch {
	case identity.ACLToken != nil && identity.ACLToken != structs.AnonymousACLToken &&
		identity.ACLToken != structs.LeaderACLToken:
		reader.kind, reader.name = bus.KindToken, identity.ACLToken.AccessorID
	case identity.Claims != nil:
		reader.kind, reader.name = bus.KindWorkload, identity.Claims.AllocationID+"/"+identity.Claims.TaskName
	default:
		return nil, structs.ErrPermissionDenied
	}
	reader.consumer = cdcConsumerName(reader.kind, reader.name)
	if reader.consumer == "" || reader.grant.Empty() {
		return nil, structs.ErrPermissionDenied
	}
	return reader, nil
}

// cdcStreamName returns the name of a change stream as the API names it;
// the admin events only for a reader with every right.
func (s *Server) cdcStreamName(stream string, reader *cdcReader) (string, error) {
	region := s.config.Region
	switch stream {
	case structs.CDCStreamEvents:
		return bus.EventStreamName(region), nil
	case structs.CDCStreamKV:
		return bus.ConsulKVChangesStreamName(region), nil
	case structs.CDCStreamAdmin:
		if !reader.grant.All {
			return "", structs.NewErrRPCCoded(http.StatusForbidden,
				"the consumers of the admin events are for management tokens only")
		}
		return bus.AdminEventStreamName(region), nil
	}
	return "", structs.NewErrRPCCodedf(http.StatusBadRequest,
		"unknown change stream %q: events, admin or kv", stream)
}

// cdcStreamSubjects are the subjects of a change stream as the API names it.
func (s *Server) cdcStreamSubjects(stream string) []string {
	prefix, region := s.busEventsPrefix(), s.config.Region
	switch stream {
	case structs.CDCStreamEvents:
		return bus.EventStreamSubjects(prefix, region)
	case structs.CDCStreamKV:
		return bus.ConsulKVChangesSubjects(prefix, region)
	}
	return []string{prefix + "." + bus.AdminSubjectToken + "." + bus.SubjectToken(region) + ".>"}
}

// cdcCheckFilters checks the filters of a consumer: at least one and at
// most CDCMaxFilters, valid subjects, within the stream and the reader's cdc
// rule, not overlapping each other.
func (s *Server) cdcCheckFilters(stream string, filters []string, reader *cdcReader) error {
	if len(filters) == 0 || len(filters) > structs.CDCMaxFilters {
		return structs.NewErrRPCCodedf(http.StatusBadRequest,
			"a consumer has 1 to %d filters", structs.CDCMaxFilters)
	}
	streamSubjects := s.cdcStreamSubjects(stream)
	for i, filter := range filters {
		if err := acl.ValidCDCSubject(filter); err != nil {
			return structs.NewErrRPCCoded(http.StatusBadRequest, err.Error())
		}
		within := func(patterns []string) bool {
			return slices.ContainsFunc(patterns, func(p string) bool { return bus.SubjectIsSubsetMatch(filter, p) })
		}
		if !within(streamSubjects) {
			return structs.NewErrRPCCodedf(http.StatusBadRequest,
				"filter %q is not a subject of the stream (%v)", filter, streamSubjects)
		}
		if !reader.grant.All && !within(reader.grant.Receive) {
			return structs.NewErrRPCCodedf(http.StatusForbidden,
				"filter %q is outside the cdc rule of the reader", filter)
		}
		for _, other := range filters[:i] {
			if bus.SubjectsCollide(filter, other) {
				return structs.NewErrRPCCodedf(http.StatusBadRequest,
					"filters %q and %q overlap", other, filter)
			}
		}
	}
	return nil
}

// cdcDeliver maps the start of a consumer.
func cdcDeliver(spec *structs.CDCConsumerSpec) (jetstream.DeliverPolicy, *time.Time, error) {
	switch spec.Deliver {
	case "", structs.CDCDeliverAll:
		return jetstream.DeliverAllPolicy, nil, nil
	case structs.CDCDeliverNew:
		return jetstream.DeliverNewPolicy, nil, nil
	case structs.CDCDeliverLastPerSubject:
		return jetstream.DeliverLastPerSubjectPolicy, nil, nil
	case structs.CDCDeliverByStartTime:
		if spec.StartTime.IsZero() {
			return 0, nil, structs.NewErrRPCCoded(http.StatusBadRequest, "deliver by-start-time needs a start time")
		}
		t := spec.StartTime
		return jetstream.DeliverByStartTimePolicy, &t, nil
	}
	return 0, nil, structs.NewErrRPCCodedf(http.StatusBadRequest,
		"unknown deliver %q: all, new, last-per-subject or by-start-time", spec.Deliver)
}

// withCDCJetStream runs fn with a JetStream context of the server's bus.
func (s *Server) withCDCJetStream(fn func(context.Context, jetstream.JetStream) error) error {
	if s.bus == nil {
		return structs.NewErrRPCCoded(http.StatusServiceUnavailable, "the servers run no system bus")
	}
	if !s.bus.JetStreamReady() {
		return structs.NewErrRPCCoded(http.StatusServiceUnavailable, "the system bus has no JetStream meta leader")
	}
	ctx, cancel := context.WithTimeout(s.shutdownCtx, cdcConsumerTimeout)
	defer cancel()
	return s.withBusJetStream(ctx, fn)
}

// ConsumerUpsert creates or changes the reader's consumer of a change
// stream. A change keeps the position of the consumer; its start can't
// change.
func (c *CDC) ConsumerUpsert(args *structs.CDCConsumerUpsertRequest, reply *structs.CDCConsumerWriteResponse) error {
	authErr := c.srv.Authenticate(c.ctx, args)
	if done, err := c.srv.forward(structs.CDCConsumerUpsertRPCMethod, args, args, reply); done {
		return err
	}
	c.srv.MeasureRPCRate("cdc", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	reader, err := c.srv.cdcRequestReader(args)
	if err != nil {
		return err
	}
	streamName, err := c.srv.cdcStreamName(args.Stream, reader)
	if err != nil {
		return err
	}
	spec := args.Consumer
	if err := c.srv.cdcCheckFilters(args.Stream, spec.Filters, reader); err != nil {
		return err
	}
	deliver, startTime, err := cdcDeliver(&spec)
	if err != nil {
		return err
	}
	ackWait := spec.AckWait
	if ackWait <= 0 {
		ackWait = cdcDefaultAckWait
	}
	maxAckPending := spec.MaxAckPending
	if maxAckPending <= 0 {
		maxAckPending = bus.EventsConsumerMaxAckPending
	}
	if maxAckPending > bus.EventsConsumerMaxAckPending {
		return structs.NewErrRPCCodedf(http.StatusBadRequest,
			"max ack pending %d is above the limit of the stream (%d)", maxAckPending, bus.EventsConsumerMaxAckPending)
	}

	return c.srv.withCDCJetStream(func(ctx context.Context, js jetstream.JetStream) error {
		stream, err := js.Stream(ctx, streamName)
		if err != nil {
			return structs.NewErrRPCCodedf(http.StatusServiceUnavailable, "the stream %s: %v", streamName, err)
		}
		info, err := stream.Info(ctx)
		if err != nil {
			return err
		}
		_, err = stream.Consumer(ctx, reader.consumer)
		exists := err == nil
		if !exists && !errors.Is(err, jetstream.ErrConsumerNotFound) {
			return err
		}
		if limit := info.Config.MaxConsumers; !exists && limit > 0 && info.State.Consumers >= limit {
			return structs.NewErrRPCCodedf(http.StatusConflict,
				"the stream %s has its maximum of %d consumers (bus.events.max_consumers)", streamName, limit)
		}
		cfg := jetstream.ConsumerConfig{
			Durable:            reader.consumer,
			Description:        "change data capture consumer of " + reader.kind + " " + reader.name,
			FilterSubjects:     spec.Filters,
			DeliverPolicy:      deliver,
			OptStartTime:       startTime,
			AckPolicy:          jetstream.AckExplicitPolicy,
			AckWait:            ackWait,
			MaxAckPending:      maxAckPending,
			MaxRequestBatch:    cdcMaxRequestBatch,
			MaxRequestMaxBytes: cdcMaxRequestMaxBytes,
			// A reader fetches across the accounts: a fetch may wait as
			// long as the bus account keeps its way back.
			MaxRequestExpires: bus.ReaderMaxFetchExpires,
			Metadata:          map[string]string{cdcReaderMeta: reader.kind + ":" + reader.name},
		}
		cons, err := stream.CreateOrUpdateConsumer(ctx, cfg)
		if err != nil {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "the consumer %s: %v", reader.consumer, err)
		}
		reply.Consumer = cdcConsumerOf(args.Stream, streamName, cons.CachedInfo())
		return nil
	})
}

// ConsumerRead reads the reader's consumer of a change stream.
func (c *CDC) ConsumerRead(args *structs.CDCConsumerReadRequest, reply *structs.CDCConsumerResponse) error {
	authErr := c.srv.Authenticate(c.ctx, args)
	if done, err := c.srv.forward(structs.CDCConsumerReadRPCMethod, args, args, reply); done {
		return err
	}
	c.srv.MeasureRPCRate("cdc", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	reader, err := c.srv.cdcRequestReader(args)
	if err != nil {
		return err
	}
	streamName, err := c.srv.cdcStreamName(args.Stream, reader)
	if err != nil {
		return err
	}
	return c.srv.withCDCJetStream(func(ctx context.Context, js jetstream.JetStream) error {
		cons, err := js.Consumer(ctx, streamName, reader.consumer)
		if errors.Is(err, jetstream.ErrConsumerNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		info, err := cons.Info(ctx)
		if err != nil {
			return err
		}
		reply.Consumer = cdcConsumerOf(args.Stream, streamName, info)
		return nil
	})
}

// ConsumerDelete deletes the reader's consumer of a change stream.
func (c *CDC) ConsumerDelete(args *structs.CDCConsumerDeleteRequest, reply *structs.CDCConsumerWriteResponse) error {
	authErr := c.srv.Authenticate(c.ctx, args)
	if done, err := c.srv.forward(structs.CDCConsumerDeleteRPCMethod, args, args, reply); done {
		return err
	}
	c.srv.MeasureRPCRate("cdc", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	reader, err := c.srv.cdcRequestReader(args)
	if err != nil {
		return err
	}
	streamName, err := c.srv.cdcStreamName(args.Stream, reader)
	if err != nil {
		return err
	}
	return c.srv.withCDCJetStream(func(ctx context.Context, js jetstream.JetStream) error {
		err := js.DeleteConsumer(ctx, streamName, reader.consumer)
		if errors.Is(err, jetstream.ErrConsumerNotFound) {
			return structs.NewErrRPCCodedf(http.StatusNotFound, "no consumer %s of the stream %s", reader.consumer, streamName)
		}
		return err
	})
}

// cdcConsumerOf converts the information of a JetStream consumer.
func cdcConsumerOf(stream, streamName string, info *jetstream.ConsumerInfo) *structs.CDCConsumer {
	cfg := info.Config
	filters := slices.Clone(cfg.FilterSubjects)
	if len(filters) == 0 && cfg.FilterSubject != "" {
		filters = []string{cfg.FilterSubject}
	}
	out := &structs.CDCConsumer{
		Stream:        stream,
		StreamName:    streamName,
		Name:          info.Name,
		Filters:       filters,
		Deliver:       cdcDeliverName(cfg.DeliverPolicy),
		AckWait:       cfg.AckWait,
		MaxAckPending: cfg.MaxAckPending,
		NumPending:    info.NumPending,
		NumAckPending: info.NumAckPending,
		Delivered:     info.Delivered.Stream,
		Created:       info.Created,
	}
	if cfg.OptStartTime != nil {
		out.StartTime = *cfg.OptStartTime
	}
	return out
}

func cdcDeliverName(p jetstream.DeliverPolicy) string {
	switch p {
	case jetstream.DeliverNewPolicy:
		return structs.CDCDeliverNew
	case jetstream.DeliverLastPerSubjectPolicy:
		return structs.CDCDeliverLastPerSubject
	case jetstream.DeliverByStartTimePolicy:
		return structs.CDCDeliverByStartTime
	}
	return structs.CDCDeliverAll
}

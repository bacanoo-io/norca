// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/bacanoo-io/norca/norca/norca/bus"
)

// The stream information of a change data capture reader. The JetStream
// clients ask for it before they read a consumer, so a reader account imports
// $JS.API.STREAM.INFO.<stream>; but the JetStream API answers that request
// with every subject of the stream and its message count when the request
// asks for it (subjects_filter), and a stream holds the changes of every
// namespace. The import therefore leads to a subject of the servers
// (bus.ReaderStreamInfoSubject), and the servers answer with the
// configuration and the state of the stream, without anything per subject
// and without reading the request.

const (
	// readerStreamInfoQueue is the queue group of the servers that answer:
	// one of them takes each request.
	readerStreamInfoQueue = "norca-reader-stream-info"

	// readerStreamInfoBuffer bounds the requests that wait for an answer.
	readerStreamInfoBuffer = 256

	// readerStreamInfoTimeout bounds the reading of the information of one
	// stream.
	readerStreamInfoTimeout = 5 * time.Second

	// readerStreamInfoType is the type of the answer, as the JetStream API
	// names it.
	readerStreamInfoType = "io.nats.jetstream.api.v1.stream_info_response"

	// serviceReplyPrefix starts the reply subject NATS makes for a request
	// that came through a service import.
	serviceReplyPrefix = "_R_."
)

// readerStreamInfoResponse is the answer in the form of the JetStream API.
type readerStreamInfoResponse struct {
	Type string `json:"type"`
	*jetstream.StreamInfo
	Error *jetstream.APIError `json:"error,omitempty"`
}

// serveReaderStreamInfo answers the stream information requests of the
// reader accounts until ctx ends. Every server runs it.
func (s *Server) serveReaderStreamInfo(ctx context.Context, js jetstream.JetStream) error {
	nc := js.Conn()
	requests := make(chan *nats.Msg, readerStreamInfoBuffer)
	sub, err := nc.ChanQueueSubscribe(bus.ReaderStreamInfoSubject+".*", readerStreamInfoQueue, requests)
	if err != nil {
		return fmt.Errorf("subscribing to %s: %w", bus.ReaderStreamInfoSubject, err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	region := s.config.Region
	streams := append(cdcReadableStreams(region), bus.AdminEventStreamName(region))
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg := <-requests:
			// Only a request that came through the import of a reader
			// account is answered: its reply subject is made by NATS. A
			// reply subject a connection of the bus account chose is never
			// published to (no rule lets one publish here, and NATS refuses
			// a client the reply subjects of the imports).
			if !strings.HasPrefix(msg.Reply, serviceReplyPrefix) {
				continue
			}
			name := strings.TrimPrefix(msg.Subject, bus.ReaderStreamInfoSubject+".")
			if !slices.Contains(streams, name) {
				continue
			}
			if err := msg.Respond(s.readerStreamInfo(ctx, js, name)); err != nil {
				return fmt.Errorf("answering a stream information request: %w", err)
			}
		}
	}
}

// readerStreamInfo is the answer for a stream: its configuration and state
// without the subjects, the deleted messages and the servers of the stream.
func (s *Server) readerStreamInfo(ctx context.Context, js jetstream.JetStream, name string) []byte {
	ctx, cancel := context.WithTimeout(ctx, readerStreamInfoTimeout)
	defer cancel()

	resp := readerStreamInfoResponse{Type: readerStreamInfoType}
	stream, err := js.Stream(ctx, name)
	if err != nil {
		s.logger.Debug("change data capture: reading the information of a stream for a reader failed", "stream", name, "error", err)
		resp.Error = &jetstream.APIError{Code: http.StatusServiceUnavailable, Description: "the stream information is not available"}
	} else {
		info := *stream.CachedInfo()
		info.State.Subjects, info.State.NumSubjects = nil, 0
		info.State.Deleted, info.State.NumDeleted = nil, 0
		info.Cluster, info.Mirror, info.Sources = nil, nil, nil
		resp.StreamInfo = &info
	}
	data, err := json.Marshal(&resp)
	if err != nil {
		// The types marshal; an answer the client can read in any case.
		return []byte(`{"type":"` + readerStreamInfoType + `","error":{"code":500,"description":"internal error"}}`)
	}
	return data
}

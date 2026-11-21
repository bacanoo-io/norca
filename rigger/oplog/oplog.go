// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package oplog makes the log of Rig a tree: every operation (a stack, a
// release in it, a step of a release, a resource) is a span, and every log
// line carries the span it belongs to.
//
// A span has an ID and the ID of its parent; the trace ID of the root
// operation is carried by the whole tree. The keys follow the key
// convention of Norca's OpenTelemetry plan (trace_id, span_id, duration),
// so the lines of Rig and of the server read alike. A JSON log keeps the
// lines flat and the tree can be rebuilt from the IDs; a text log can
// indent them by depth.
//
// Spans travel in a context. [Start] opens one and logs its start, [Span.End]
// logs its end with the duration and the outcome. [NewHandler] wraps a
// slog.Handler so that every line logged with a context carries the IDs of
// the span in it. Log with the Context variants of slog (InfoContext ...),
// or the line belongs to no span.
//
// The IDs have the sizes of OpenTelemetry (16 bytes for a trace, 8 for a
// span), so that a later bridge can open OpenTelemetry spans with the same
// IDs when the host has a tracer (design 12.2).
package oplog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"time"
)

// The keys oplog adds to log lines.
const (
	// KeyTraceID holds the trace ID of the root operation.
	KeyTraceID = "trace_id"
	// KeySpanID holds the ID of the span a line belongs to.
	KeySpanID = "span_id"
	// KeyParentSpanID holds the ID of the parent span; absent on a root.
	KeyParentSpanID = "parent_span_id"
	// KeyDepth holds the depth of the span: 0 for a root.
	KeyDepth = "depth"
	// KeyOp holds the name of the operation, on its start and end lines.
	KeyOp = "op"
	// KeyDuration holds the duration of an operation, on its end line.
	KeyDuration = "duration"
	// KeyOutcome holds "ok" or "error", on the end line of an operation.
	KeyOutcome = "outcome"
)

// The messages of the start and end lines.
const (
	MsgStarted  = "started"
	MsgFinished = "finished"
)

// The outcomes of an operation.
const (
	OutcomeOK    = "ok"
	OutcomeError = "error"
)

// Span is an operation in the tree. It is created by [Start] and ended once
// by [Span.End].
type Span struct {
	traceID  [16]byte
	id       [8]byte
	parentID [8]byte
	isRoot   bool
	depth    int
	name     string
	level    slog.Level
	start    time.Time
	logger   *slog.Logger
}

type spanKey struct{}

// Start opens a span named name as a child of the span in ctx, or as a root
// when ctx has none, and logs its start line at level with args. It returns
// a context that carries the new span; log the lines of the operation with
// it, and pass it to the operations below.
//
// The name is the kind of operation, constant and low in cardinality
// ("stack.apply", "release.plan"); what varies goes into args
// ("release", name).
func Start(ctx context.Context, logger *slog.Logger, level slog.Level, name string, args ...any) (context.Context, *Span) {
	s := &Span{
		name:   name,
		level:  level,
		start:  time.Now(),
		logger: logger,
		isRoot: true,
	}

	fill(s.id[:])

	if parent, ok := FromContext(ctx); ok {
		s.traceID = parent.traceID
		s.parentID = parent.id
		s.isRoot = false
		s.depth = parent.depth + 1
	} else {
		fill(s.traceID[:])
	}

	ctx = context.WithValue(ctx, spanKey{}, s)
	logger.Log(ctx, level, MsgStarted, append([]any{KeyOp, name}, args...)...)

	return ctx, s
}

// End logs the end line of the span with its duration and outcome. Pass the
// error the operation returns: the line then says outcome=error, without
// the error itself, which the caller reports once (golang-error-handling:
// an error is logged or returned, not both). Use the context Start
// returned.
func (s *Span) End(ctx context.Context, err error) {
	outcome := OutcomeOK
	if err != nil {
		outcome = OutcomeError
	}

	s.logger.Log(ctx, s.level, MsgFinished,
		KeyOp, s.name,
		KeyDuration, time.Since(s.start),
		KeyOutcome, outcome,
	)
}

// TraceID returns the trace ID in hex.
func (s *Span) TraceID() string {
	return hex.EncodeToString(s.traceID[:])
}

// ID returns the span ID in hex.
func (s *Span) ID() string {
	return hex.EncodeToString(s.id[:])
}

// FromContext returns the span ctx carries.
func FromContext(ctx context.Context) (*Span, bool) {
	s, ok := ctx.Value(spanKey{}).(*Span)

	return s, ok
}

// attrs returns the attributes NewHandler adds to a line of the span.
func (s *Span) attrs() []slog.Attr {
	attrs := []slog.Attr{
		slog.String(KeyTraceID, s.TraceID()),
		slog.String(KeySpanID, s.ID()),
	}

	if !s.isRoot {
		attrs = append(attrs, slog.String(KeyParentSpanID, hex.EncodeToString(s.parentID[:])))
	}

	return append(attrs, slog.Int(KeyDepth, s.depth))
}

// fill fills b with random bytes.
func fill(b []byte) {
	// crypto/rand.Read does not fail since Go 1.24; a failure is a broken
	// system, not an error to handle.
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
}

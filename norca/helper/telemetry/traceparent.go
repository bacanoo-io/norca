// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"encoding/hex"

	"go.opentelemetry.io/otel/trace"
)

// traceParentLen is the length of a version 00 W3C traceparent:
// "00-" + 32 hex digits + "-" + 16 hex digits + "-" + 2 hex digits.
const traceParentLen = 55

// FormatTraceParent formats a span context as a version 00 W3C traceparent
// ("00-<trace id>-<span id>-<flags>"). It returns "" for an invalid span
// context. Only the sampled flag is carried; the trace state is not.
func FormatTraceParent(sc trace.SpanContext) string {
	if !sc.IsValid() {
		return ""
	}
	var b [traceParentLen]byte
	tid, sid := sc.TraceID(), sc.SpanID()
	b[0], b[1], b[2] = '0', '0', '-'
	hex.Encode(b[3:35], tid[:])
	b[35] = '-'
	hex.Encode(b[36:52], sid[:])
	b[52] = '-'
	b[53] = '0'
	b[54] = '0'
	if sc.IsSampled() {
		b[54] = '1'
	}
	return string(b[:])
}

// ParseTraceParent parses a version 00 W3C traceparent into a remote span
// context. It accepts only the exact form FormatTraceParent writes (lower
// case hex, no trailing fields, non-zero IDs), because the traceparents it
// reads come from Norca's own RPC arguments and Raft log entries; anything
// else yields false.
func ParseTraceParent(s string) (trace.SpanContext, bool) {
	if len(s) != traceParentLen || s[0] != '0' || s[1] != '0' || s[2] != '-' || s[35] != '-' || s[52] != '-' {
		return trace.SpanContext{}, false
	}
	var tid trace.TraceID
	var sid trace.SpanID
	if !decodeLowerHex(tid[:], s[3:35]) || !decodeLowerHex(sid[:], s[36:52]) {
		return trace.SpanContext{}, false
	}
	var flags [1]byte
	if !decodeLowerHex(flags[:], s[53:55]) {
		return trace.SpanContext{}, false
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.TraceFlags(flags[0]) & trace.FlagsSampled,
		Remote:     true,
	})
	if !sc.IsValid() {
		return trace.SpanContext{}, false
	}
	return sc, true
}

// decodeLowerHex decodes the lower case hex digits of src into dst, which
// has half the length of src, without allocating.
func decodeLowerHex(dst []byte, src string) bool {
	for i := range dst {
		hi, ok1 := fromLowerHex(src[2*i])
		lo, ok2 := fromLowerHex(src[2*i+1])
		if !ok1 || !ok2 {
			return false
		}
		dst[i] = hi<<4 | lo
	}
	return true
}

func fromLowerHex(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	}
	return 0, false
}

// TraceParentFromContext returns the traceparent of the span in ctx, or ""
// when ctx carries no valid span context. The HTTP API sets it in the RPC
// arguments it sends, so the servers continue the request's trace.
func TraceParentFromContext(ctx context.Context) string {
	return FormatTraceParent(trace.SpanContextFromContext(ctx))
}

// ContextWithTraceParent returns ctx with the remote span context of a
// traceparent, or ctx itself when tp is empty or malformed.
func ContextWithTraceParent(ctx context.Context, tp string) context.Context {
	if tp == "" {
		return ctx
	}
	sc, ok := ParseTraceParent(tp)
	if !ok {
		return ctx
	}
	return trace.ContextWithRemoteSpanContext(ctx, sc)
}

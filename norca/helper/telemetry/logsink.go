// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

// The attributes of the agent's log records.
const (
	// AttrLogger is the name of the hclog logger that wrote the line.
	AttrLogger = "norca.logger"

	// attrExceptionStacktrace holds a stack trace hclog captured.
	attrExceptionStacktrace = "exception.stacktrace"

	// attrExceptionMessage and attrExceptionType describe the error of a
	// line; the log SDK sets them from the record's error, except on a
	// record with an exception.* attribute already (a stack trace), where
	// the sink sets them itself.
	attrExceptionMessage = "exception.message"
	attrExceptionType    = "exception.type"

	// logKeyError is the key of the error of a log line; its value becomes
	// the error of the record (exception.message and exception.type).
	logKeyError = "error"
)

// LogKeyOTLPInternal marks the log lines that report a failure of the OTLP
// export of the logs (or an internal error of the OpenTelemetry SDK). The
// log sink drops these lines, so a failing log export does not feed
// itself; they still go to the agent's other log outputs.
const LogKeyOTLPInternal = "otlp_internal"

// earlyLogRecords bounds the log records the sink keeps until it is bound,
// once the agent's identity is complete (the agent's setup); the ones
// beyond it are dropped.
const earlyLogRecords = 1024

// The severities of the hclog levels, as the OpenTelemetry log data model
// numbers them.
var logSeverities = [...]struct {
	number otellog.Severity
	text   string
}{
	hclog.Trace: {otellog.SeverityTrace, "TRACE"},
	hclog.Debug: {otellog.SeverityDebug, "DEBUG"},
	hclog.Info:  {otellog.SeverityInfo, "INFO"},
	hclog.Warn:  {otellog.SeverityWarn, "WARN"},
	hclog.Error: {otellog.SeverityError, "ERROR"},
}

// LogSink is the hclog sink of the logs signal: registered on the agent's
// intercept logger, it turns every log line at or above its level into an
// OpenTelemetry log record. It is called synchronously by every logger of
// the agent, under the intercept logger's lock, so a line below its level
// costs one atomic load.
//
// Until it is bound (Providers.Identify, once the agent runs) it keeps up
// to earlyLogRecords records, which it emits then; the lines of the agent's
// setup are exported too, with the agent's complete resource.
type LogSink struct {
	level  atomic.Int32
	closed atomic.Bool
	now    func() time.Time

	// logger is set once, when the agent's identity is complete.
	logger atomic.Pointer[otelLoggerRef]

	mu           sync.Mutex
	early        []earlyLogRecord
	earlyDropped int
}

var _ hclog.SinkAdapter = (*LogSink)(nil)

type otelLoggerRef struct{ otellog.Logger }

// earlyLogRecord is a record made before the sink was bound.
type earlyLogRecord struct {
	record otellog.Record
	span   trace.SpanContext
}

// NewLogSink returns a sink exporting the lines at or above level. It
// exports nothing until it is bound to a logger.
func NewLogSink(level hclog.Level) *LogSink {
	s := &LogSink{now: time.Now}
	s.SetLevel(level)
	return s
}

// SetLevel changes the minimum level exported (a reload of logs.level).
func (s *LogSink) SetLevel(level hclog.Level) {
	s.level.Store(int32(level))
}

// Level returns the minimum level exported.
func (s *LogSink) Level() hclog.Level {
	return hclog.Level(s.level.Load())
}

// Bind makes the sink emit to logger from now on, after the records it kept
// until now.
func (s *LogSink) Bind(logger otellog.Logger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.early {
		logger.Emit(spanContext(r.span), r.record)
	}
	s.early = nil
	s.logger.Store(&otelLoggerRef{logger})
}

// close stops the sink: the providers shut down.
func (s *LogSink) close() {
	s.closed.Store(true)
}

// EarlyDropped returns the number of records dropped before the sink was
// bound because it kept earlyLogRecords already.
func (s *LogSink) EarlyDropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.earlyDropped
}

// Accept implements hclog.SinkAdapter.
func (s *LogSink) Accept(name string, level hclog.Level, msg string, args ...any) {
	if int32(level) < s.level.Load() || level > hclog.Error || s.closed.Load() {
		return
	}
	if isOTLPInternal(args) {
		return
	}
	record, span := s.record(name, level, msg, args)
	if ref := s.logger.Load(); ref != nil {
		ref.Emit(spanContext(span), record)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ref := s.logger.Load(); ref != nil {
		// bound since the check above
		ref.Emit(spanContext(span), record)
		return
	}
	if len(s.early) >= earlyLogRecords {
		s.earlyDropped++
		return
	}
	s.early = append(s.early, earlyLogRecord{record: record, span: span})
}

// isOTLPInternal reports whether the key/value arguments of a log line hold
// the marker of the OTLP export's own failure lines.
func isOTLPInternal(args []any) bool {
	for i := 0; i+1 < len(args); i += 2 {
		if k, ok := args[i].(string); ok && k == LogKeyOTLPInternal {
			return true
		}
	}
	return false
}

// spanContext returns a context carrying the trace and span ID of a log
// line, which the SDK copies into the record.
func spanContext(sc trace.SpanContext) context.Context {
	if !sc.TraceID().IsValid() {
		return context.Background()
	}
	return trace.ContextWithSpanContext(context.Background(), sc)
}

// record converts a log line: severity, the message as body, the logger
// name, the key/values as typed attributes, the error as the record's
// error, and trace_id/span_id as the trace context of the record.
func (s *LogSink) record(name string, level hclog.Level, msg string, args []any) (otellog.Record, trace.SpanContext) {
	var r otellog.Record
	r.SetTimestamp(s.now())
	sev := logSeverities[level]
	r.SetSeverity(sev.number)
	r.SetSeverityText(sev.text)
	body, _ := cutUTF8(msg, logValueLengthMax)
	r.SetBody(otellog.StringValue(body))
	if name != "" {
		r.AddAttributes(otellog.String(AttrLogger, name))
	}

	var span trace.SpanContextConfig
	var hasStack bool
	n := len(args)
	if n%2 != 0 {
		// hclog logs an odd last value as a stack trace or under its
		// missing key
		last := args[n-1]
		n--
		if st, ok := last.(hclog.CapturedStacktrace); ok {
			r.AddAttributes(otellog.String(attrExceptionStacktrace, string(st)))
			hasStack = true
		} else {
			r.AddAttributes(otellog.KeyValue{Key: hclog.MissingKey, Value: logValue(last)})
		}
	}
	for i := 0; i < n; i += 2 {
		key := logKey(args[i])
		value := args[i+1]
		switch key {
		case LogKeyTraceID:
			if id, ok := hexTraceID(value); ok {
				span.TraceID = id
				continue
			}
		case LogKeySpanID:
			if id, ok := hexSpanID(value); ok {
				span.SpanID = id
				continue
			}
		case logKeyError:
			if err, ok := value.(error); ok && err != nil {
				r.SetErr(err)
				if hasStack {
					// the SDK skips its exception attributes next to
					// exception.stacktrace
					r.AddAttributes(exceptionAttrs(err)...)
				}
				continue
			}
		}
		r.AddAttributes(otellog.KeyValue{Key: key, Value: logValue(value)})
	}
	return r, trace.NewSpanContext(span)
}

// exceptionAttrs returns exception.message and exception.type of err as the
// log SDK makes them: the type is the ErrorType() of err, or the package
// and name of its type, looking through fmt.Errorf wrappers.
func exceptionAttrs(err error) []otellog.KeyValue {
	var attrs []otellog.KeyValue
	if msg := err.Error(); msg != "" {
		attrs = append(attrs, otellog.String(attrExceptionMessage, msg))
	}
	if typ := errorType(err); typ != "" {
		attrs = append(attrs, otellog.String(attrExceptionType, typ))
	}
	return attrs
}

// fmtWrapErrorType is the type of an error made by fmt.Errorf with one %w.
var fmtWrapErrorType = reflect.TypeOf(fmt.Errorf("w: %w", errors.New("e")))

func errorType(err error) string {
	if et, ok := err.(interface{ ErrorType() string }); ok {
		if s := et.ErrorType(); s != "" {
			return s
		}
	}
	for reflect.TypeOf(err) == fmtWrapErrorType {
		inner := errors.Unwrap(err)
		if inner == nil {
			break
		}
		err = inner
	}
	t := reflect.TypeOf(err)
	if t == nil {
		return ""
	}
	if pkg, name := t.PkgPath(), t.Name(); pkg != "" && name != "" {
		return pkg + "." + name
	}
	return t.String()
}

func logKey(k any) string {
	if s, ok := k.(string); ok {
		return s
	}
	return fmt.Sprint(k)
}

func hexTraceID(v any) (trace.TraceID, bool) {
	s, ok := v.(string)
	if !ok {
		return trace.TraceID{}, false
	}
	id, err := trace.TraceIDFromHex(s)
	return id, err == nil
}

func hexSpanID(v any) (trace.SpanID, bool) {
	s, ok := v.(string)
	if !ok {
		return trace.SpanID{}, false
	}
	id, err := trace.SpanIDFromHex(s)
	return id, err == nil
}

// logValue converts the value of a log line to a typed attribute value:
// strings, booleans, integers and floats keep their type, a duration is a
// float of milliseconds, a time is RFC 3339, an error and a fmt.Stringer
// are their text, hclog's format helpers are applied, anything else is
// formatted with %v.
func logValue(v any) otellog.Value {
	switch v := v.(type) {
	case nil:
		return otellog.Value{}
	case string:
		return otellog.StringValue(v)
	case bool:
		return otellog.BoolValue(v)
	case int:
		return otellog.IntValue(v)
	case int8:
		return otellog.Int64Value(int64(v))
	case int16:
		return otellog.Int64Value(int64(v))
	case int32:
		return otellog.Int64Value(int64(v))
	case int64:
		return otellog.Int64Value(v)
	case uint8:
		return otellog.Int64Value(int64(v))
	case uint16:
		return otellog.Int64Value(int64(v))
	case uint32:
		return otellog.Int64Value(int64(v))
	case uint:
		return uint64Value(uint64(v))
	case uint64:
		return uint64Value(v)
	case float32:
		return otellog.Float64Value(float64(v))
	case float64:
		return otellog.Float64Value(v)
	case time.Duration:
		return otellog.Float64Value(float64(v) / float64(time.Millisecond))
	case time.Time:
		return otellog.StringValue(v.Format(time.RFC3339Nano))
	case []byte:
		return otellog.BytesValue(v)
	case []string:
		values := make([]otellog.Value, len(v))
		for i, s := range v {
			values[i] = otellog.StringValue(s)
		}
		return otellog.SliceValue(values...)
	case hclog.Hex:
		return otellog.IntValue(int(v))
	case hclog.Octal:
		return otellog.IntValue(int(v))
	case hclog.Binary:
		return otellog.IntValue(int(v))
	case hclog.Quote:
		return otellog.StringValue(string(v))
	case hclog.Format:
		return otellog.StringValue(formatValue(v))
	case error, fmt.Stringer:
		// fmt recovers from a panicking Error or String method (a nil
		// receiver)
		return otellog.StringValue(fmt.Sprint(v))
	default:
		return otellog.StringValue(fmt.Sprintf("%v", v))
	}
}

// uint64Value keeps an unsigned integer that int64 can't hold as its text.
func uint64Value(v uint64) otellog.Value {
	if v > math.MaxInt64 {
		return otellog.StringValue(fmt.Sprint(v))
	}
	return otellog.Int64Value(int64(v))
}

// formatValue applies an hclog.Format: a format and its arguments.
func formatValue(f hclog.Format) string {
	if len(f) == 0 {
		return ""
	}
	format, ok := f[0].(string)
	if !ok {
		return fmt.Sprint(f...)
	}
	return fmt.Sprintf(format, f[1:]...)
}

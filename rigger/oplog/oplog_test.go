// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package oplog_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/rigger/oplog"
)

// newLogger returns a logger whose JSON lines go to buf, with the IDs of
// oplog.
func newLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(oplog.NewHandler(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

// lines decodes the JSON lines of buf.
func lines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()

	var out []map[string]any

	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %q is not JSON: %v", line, err)
		}

		out = append(out, m)
	}

	return out
}

func TestStart_Tree(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := newLogger(&buf)

	ctx, root := oplog.Start(t.Context(), logger, slog.LevelInfo, "stack.apply", "stack", "platform")
	childCtx, child := oplog.Start(ctx, logger, slog.LevelDebug, "release.apply", "release", "postgres")
	logger.DebugContext(childCtx, "resource applied", "kind", "job", "name", "postgres")
	child.End(childCtx, nil)
	root.End(ctx, nil)

	got := lines(t, &buf)
	if len(got) != 5 {
		t.Fatalf("got %d lines, want 5: %s", len(got), buf.String())
	}

	tests := []struct {
		name     string
		line     map[string]any
		msg      string
		spanID   string
		parentID any
		depth    float64
	}{
		{name: "root started", line: got[0], msg: oplog.MsgStarted, spanID: root.ID(), parentID: nil, depth: 0},
		{name: "child started", line: got[1], msg: oplog.MsgStarted, spanID: child.ID(), parentID: root.ID(), depth: 1},
		{name: "line of the child", line: got[2], msg: "resource applied", spanID: child.ID(), parentID: root.ID(), depth: 1},
		{name: "child finished", line: got[3], msg: oplog.MsgFinished, spanID: child.ID(), parentID: root.ID(), depth: 1},
		{name: "root finished", line: got[4], msg: oplog.MsgFinished, spanID: root.ID(), parentID: nil, depth: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.line["msg"] != tt.msg {
				t.Errorf("msg = %v, want %q", tt.line["msg"], tt.msg)
			}

			if tt.line[oplog.KeyTraceID] != root.TraceID() {
				t.Errorf("trace_id = %v, want the root's %q", tt.line[oplog.KeyTraceID], root.TraceID())
			}

			if tt.line[oplog.KeySpanID] != tt.spanID {
				t.Errorf("span_id = %v, want %q", tt.line[oplog.KeySpanID], tt.spanID)
			}

			if tt.line[oplog.KeyParentSpanID] != tt.parentID {
				t.Errorf("parent_span_id = %v, want %v", tt.line[oplog.KeyParentSpanID], tt.parentID)
			}

			if tt.line[oplog.KeyDepth] != tt.depth {
				t.Errorf("depth = %v, want %v", tt.line[oplog.KeyDepth], tt.depth)
			}
		})
	}

	if got[0][oplog.KeyOp] != "stack.apply" || got[0]["stack"] != "platform" {
		t.Errorf("start line of the root = %v, want op stack.apply and its attributes", got[0])
	}

	if got[4][oplog.KeyOutcome] != oplog.OutcomeOK || got[4][oplog.KeyDuration] == nil {
		t.Errorf("end line of the root = %v, want outcome ok and a duration", got[4])
	}
}

func TestSpan_EndWithError(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := newLogger(&buf)
	ctx, span := oplog.Start(t.Context(), logger, slog.LevelInfo, "release.plan")
	span.End(ctx, errors.New("plan refused: quota exceeded"))

	got := lines(t, &buf)
	if got[1][oplog.KeyOutcome] != oplog.OutcomeError {
		t.Errorf("outcome = %v, want error", got[1][oplog.KeyOutcome])
	}

	if strings.Contains(buf.String(), "quota exceeded") {
		t.Errorf("the end line holds the error, which the caller reports: %s", buf.String())
	}
}

func TestHandler_WithoutSpan(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	newLogger(&buf).InfoContext(t.Context(), "no operation")

	line := lines(t, &buf)[0]
	if _, ok := line[oplog.KeySpanID]; ok {
		t.Errorf("a line without a span got span IDs: %v", line)
	}
}

func TestNewHandler_Once(t *testing.T) {
	t.Parallel()

	h := oplog.NewHandler(slog.DiscardHandler)
	if oplog.NewHandler(h) != h {
		t.Error("NewHandler wrapped its own handler again")
	}

	if h.Unwrap() != slog.DiscardHandler {
		t.Error("Unwrap does not return the wrapped handler")
	}
}

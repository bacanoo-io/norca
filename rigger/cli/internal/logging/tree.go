// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package logging holds the log handlers of the rig program.
package logging

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"

	"github.com/bacanoo-io/norca/rigger/oplog"
)

// timeFormat is the time of a tree line: the day is the day of the run.
const timeFormat = "15:04:05.000"

// hidden are the keys a tree line does not show: the time, the level and
// the message have places of their own, the depth is the indentation, the
// operation leads the line, and the IDs are for the JSON log.
var hidden = map[string]bool{
	slog.TimeKey:          true,
	slog.LevelKey:         true,
	slog.MessageKey:       true,
	oplog.KeyOp:           true,
	oplog.KeyDepth:        true,
	oplog.KeyTraceID:      true,
	oplog.KeySpanID:       true,
	oplog.KeyParentSpanID: true,
}

// Tree is a slog.Handler that writes a line for a person per record: the
// time, the level, then, indented by the depth of its operation
// (package oplog), the operation, the message and the attributes:
//
//	22:10:07.624 INFO  stack.apply started stack=platform
//	22:10:07.701 INFO    release.apply started release=postgres
//	22:10:09.310 DEBUG   resource applied kind=job name=postgres
//	22:10:09.312 INFO    release.apply finished duration=1.611s outcome=ok
//
// It is safe for concurrent use; lines of operations that run at the same
// time interleave, and the JSON log tells them apart by their IDs.
type Tree struct {
	mu    *sync.Mutex
	w     io.Writer
	buf   *bytes.Buffer
	attrs slog.Handler
	level slog.Leveler
}

var _ slog.Handler = (*Tree)(nil)

// NewTree returns a Tree that writes to w the records at level and above
// (slog.LevelInfo when level is nil).
func NewTree(w io.Writer, level slog.Leveler) *Tree {
	if level == nil {
		level = slog.LevelInfo
	}

	buf := &bytes.Buffer{}

	return &Tree{
		mu:  &sync.Mutex{},
		w:   w,
		buf: buf,
		// The text handler formats the attributes only; the level is
		// filtered by Enabled.
		attrs: slog.NewTextHandler(buf, &slog.HandlerOptions{
			Level:       slog.Level(math.MinInt),
			ReplaceAttr: hide,
		}),
		level: level,
	}
}

// Enabled reports whether level is at the level of the Tree or above.
func (t *Tree) Enabled(_ context.Context, level slog.Level) bool {
	return level >= t.level.Level()
}

// Handle writes the line of r.
func (t *Tree) Handle(ctx context.Context, r slog.Record) error {
	depth, op := 0, ""

	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case oplog.KeyDepth:
			depth = int(a.Value.Int64())
		case oplog.KeyOp:
			op = a.Value.String()
		}

		return true
	})

	t.mu.Lock()
	defer t.mu.Unlock()

	t.buf.Reset()

	if err := t.attrs.Handle(ctx, r); err != nil {
		return err
	}

	var line strings.Builder

	if !r.Time.IsZero() {
		line.WriteString(r.Time.Format(timeFormat))
		line.WriteByte(' ')
	}

	line.WriteString(r.Level.String())
	line.WriteString(strings.Repeat(" ", max(1, 6-len(r.Level.String()))))
	line.WriteString(strings.Repeat("  ", max(0, depth)))

	if op != "" {
		line.WriteString(op)
		line.WriteByte(' ')
	}

	line.WriteString(r.Message)

	if attrs := strings.TrimSpace(t.buf.String()); attrs != "" {
		line.WriteByte(' ')
		line.WriteString(attrs)
	}

	line.WriteByte('\n')

	_, err := io.WriteString(t.w, line.String())

	return err
}

// WithAttrs returns a Tree whose lines carry the attributes.
func (t *Tree) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *t
	c.attrs = t.attrs.WithAttrs(attrs)

	return &c
}

// WithGroup returns a Tree whose attributes are in the group.
func (t *Tree) WithGroup(name string) slog.Handler {
	c := *t
	c.attrs = t.attrs.WithGroup(name)

	return &c
}

// hide drops the attributes a tree line shows elsewhere or not at all.
func hide(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && hidden[a.Key] {
		return slog.Attr{}
	}

	return a
}

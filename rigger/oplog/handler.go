// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package oplog

import (
	"context"
	"log/slog"
)

// Handler adds the IDs of the span in the context of a record to the
// record, and passes it to the handler it wraps.
type Handler struct {
	next slog.Handler
}

var _ slog.Handler = (*Handler)(nil)

// NewHandler wraps next. A handler that is already a *Handler is returned
// as it is, so that the IDs are not added twice.
func NewHandler(next slog.Handler) *Handler {
	if h, ok := next.(*Handler); ok {
		return h
	}

	return &Handler{next: next}
}

// Unwrap returns the handler h wraps.
func (h *Handler) Unwrap() slog.Handler {
	return h.next
}

// Enabled reports whether the wrapped handler handles records at level.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle adds the IDs of the span in ctx, if any, and passes the record on.
// Under a group (WithGroup) the IDs land inside the group; the engine logs
// without groups.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if s, ok := FromContext(ctx); ok {
		r = r.Clone()
		r.AddAttrs(s.attrs()...)
	}

	return h.next.Handle(ctx, r)
}

// WithAttrs returns a handler whose wrapped handler has the attributes.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{next: h.next.WithAttrs(attrs)}
}

// WithGroup returns a handler whose wrapped handler has the group.
func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{next: h.next.WithGroup(name)}
}

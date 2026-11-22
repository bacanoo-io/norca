// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger

import (
	"context"
	"errors"
	"runtime/debug"

	"github.com/hashicorp/hcl/v2"
)

// The engine is the boundary of a panic (design 12.4): a server embeds it,
// and evaluates what others wrote in a process that a panic would end. The
// operations of the engine defer these and turn a panic into ErrInternal.
// The text of a panic may hold a sensitive value, so only the stack is
// logged. A stack overflow and a lack of memory are not panics: the limits
// keep those away.

// ErrInternal is the error of an operation that panicked.
var ErrInternal = errors.New("internal error of rigger; the stack trace is in the log")

// recoverDiags, deferred, turns a panic of the operation op into
// diagnostics.
func (e *Engine) recoverDiags(ctx context.Context, op string, out *hcl.Diagnostics) {
	if r := recover(); r != nil {
		e.logPanic(ctx, op)

		*out = errorDiags("Internal error", ErrInternal)
	}
}

// recoverErr, deferred, turns a panic of the operation op into an error.
func (e *Engine) recoverErr(ctx context.Context, op string, out *error) {
	if r := recover(); r != nil {
		e.logPanic(ctx, op)

		*out = ErrInternal
	}
}

// logPanic logs where an operation panicked: the one error the engine logs,
// because the caller gets no stack.
func (e *Engine) logPanic(ctx context.Context, op string) {
	e.logger.ErrorContext(ctx, "recovered a panic", "op", op, "stack", string(debug.Stack()))
}

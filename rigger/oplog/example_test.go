// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package oplog_test

import (
	"context"
	"log/slog"
	"os"

	"github.com/bacanoo-io/norca/rigger/oplog"
)

// The operations of a stack apply form a tree. The example leaves out the
// time, the random IDs and the durations so that its output is fixed.
func ExampleStart() {
	hide := map[string]bool{
		slog.TimeKey: true, oplog.KeyTraceID: true, oplog.KeySpanID: true,
		oplog.KeyParentSpanID: true, oplog.KeyDuration: true,
	}
	text := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if hide[a.Key] {
				return slog.Attr{}
			}

			return a
		},
	})
	logger := slog.New(oplog.NewHandler(text))

	ctx, stack := oplog.Start(context.Background(), logger, slog.LevelInfo, "stack.apply", "stack", "platform")
	relCtx, rel := oplog.Start(ctx, logger, slog.LevelInfo, "release.apply", "release", "postgres")
	logger.DebugContext(relCtx, "resource applied", "kind", "job", "name", "postgres")
	rel.End(relCtx, nil)
	stack.End(ctx, nil)
	// Output:
	// level=INFO msg=started op=stack.apply stack=platform depth=0
	// level=INFO msg=started op=release.apply release=postgres depth=1
	// level=DEBUG msg="resource applied" kind=job name=postgres depth=1
	// level=INFO msg=finished op=release.apply outcome=ok depth=1
	// level=INFO msg=finished op=stack.apply outcome=ok depth=0
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package logging

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/rigger/oplog"
)

// withoutTime cuts the time from the lines of buf.
func withoutTime(buf *bytes.Buffer) []string {
	var out []string

	for line := range strings.SplitSeq(strings.TrimRight(buf.String(), "\n"), "\n") {
		_, rest, _ := strings.Cut(line, " ")
		out = append(out, rest)
	}

	return out
}

func TestTree_Handle(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := slog.New(oplog.NewHandler(NewTree(&buf, slog.LevelDebug)))

	ctx, stack := oplog.Start(t.Context(), logger, slog.LevelInfo, "stack.apply", "stack", "platform")
	relCtx, rel := oplog.Start(ctx, logger, slog.LevelInfo, "release.apply", "release", "postgres")
	logger.DebugContext(relCtx, "resource applied", "kind", "job", "name", "postgres")
	rel.End(relCtx, errors.New("timeout"))
	logger.InfoContext(ctx, "pruned", "releases", 0)
	stack.End(ctx, nil)

	got := withoutTime(&buf)
	expected := []string{
		"INFO  stack.apply started stack=platform",
		"INFO    release.apply started release=postgres",
		"DEBUG   resource applied kind=job name=postgres",
		"INFO    release.apply finished duration=",
		"INFO  pruned releases=0",
		"INFO  stack.apply finished duration=",
	}

	if len(got) != len(expected) {
		t.Fatalf("got %d lines, want %d:\n%s", len(got), len(expected), buf.String())
	}

	for i := range expected {
		if !strings.HasPrefix(got[i], expected[i]) {
			t.Errorf("line %d = %q, want it to start with %q", i, got[i], expected[i])
		}
	}

	if !strings.HasSuffix(got[3], "outcome=error") || !strings.HasSuffix(got[5], "outcome=ok") {
		t.Errorf("the end lines lack their outcome:\n%s", buf.String())
	}

	if strings.Contains(buf.String(), oplog.KeySpanID) {
		t.Errorf("the tree shows IDs:\n%s", buf.String())
	}
}

func TestTree_Level(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := slog.New(NewTree(&buf, nil))
	logger.DebugContext(t.Context(), "hidden")
	logger.WarnContext(t.Context(), "shown")

	if got := withoutTime(&buf); len(got) != 1 || got[0] != "WARN  shown" {
		t.Errorf("lines = %q, want only the warning", got)
	}
}

func TestTree_WithAttrs(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := slog.New(NewTree(&buf, nil)).With("stack", "platform").WithGroup("plan")
	logger.InfoContext(t.Context(), "planned", "changes", 2)

	if got := withoutTime(&buf); len(got) != 1 || got[0] != "INFO  planned stack=platform plan.changes=2" {
		t.Errorf("lines = %q", got)
	}
}

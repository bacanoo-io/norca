// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"
	"time"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
)

// The last records are written before the end of an operation, so they may
// come between a read of its log and a read of its phase. A reader that
// follows the pages until Finished has them all.
func TestRiggerLogPage_LastRecords(t *testing.T) {
	ci.Parallel(t)

	for _, tt := range []struct {
		name   string
		before int // the records at the first read
		wait   time.Duration
	}{
		{name: "none yet, no wait"},
		{name: "none yet, waiting", wait: 5 * time.Second},
		{name: "some, no wait", before: 2},
		{name: "some, waiting", before: 2, wait: 5 * time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const all = 4

			stored, phase := tt.before, riggerOpRunning

			page := riggerLogPage{
				limit: 10,
				phase: func() string { return phase },
				read: func(after uint64, limit int, _ time.Duration) ([]RiggerLogLine, error) {
					var out []RiggerLogLine
					for seq := int(after) + 1; seq <= stored && len(out) < limit; seq++ {
						out = append(out, RiggerLogLine{Seq: uint64(seq)})
					}

					// The operation ends right after the read.
					stored, phase = all, riggerOpSucceeded

					return out, nil
				},
			}

			got := 0

			for range all + 1 {
				lines, _, finished, err := page.next(tt.wait)
				must.NoError(t, err)

				got += len(lines)
				if len(lines) > 0 {
					page.after = lines[len(lines)-1].Seq
				}

				if finished {
					break
				}
			}

			must.Eq(t, all, got, must.Sprint("the records a reader had when it was told Finished"))
		})
	}
}

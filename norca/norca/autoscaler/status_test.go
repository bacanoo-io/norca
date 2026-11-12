// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package autoscaler

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

func TestStatusBook_Record(t *testing.T) {
	ci.Parallel(t)

	base := time.Unix(1_700_000_000, 0)
	key := targetKey{namespace: "default", job: "shop", group: "web"}
	heldAt := func(d time.Duration, reason string) *Evaluation {
		eval := &Evaluation{Time: base.Add(d), Desired: 6, Running: 4}
		eval.held(reason, "")
		return eval
	}

	t.Run("the last evaluation is kept and copied out", func(t *testing.T) {
		book := newStatusBook(time.Minute, nil)
		must.Nil(t, book.get("policy"))

		book.record("policy", key, &Evaluation{
			Time:    base,
			Outcome: OutcomeNone,
			Checks:  []CheckEvaluation{{Name: "cpu", Value: 50, HasValue: true, Direction: "none"}},
		})
		got := book.get("policy")
		must.NotNil(t, got)
		must.Eq(t, OutcomeNone, got.Outcome)
		must.Eq(t, "none", got.Direction)

		// What is handed out is a copy.
		got.Checks[0].Name = "changed"
		must.Eq(t, "cpu", book.get("policy").Checks[0].Name)
	})

	t.Run("a hold keeps the time it began", func(t *testing.T) {
		book := newStatusBook(time.Hour, nil)
		book.record("policy", key, heldAt(0, HoldSettling))
		book.record("policy", key, heldAt(30*time.Second, HoldSettling))
		must.Eq(t, base, book.get("policy").HoldSince)

		// Another reason is another hold.
		book.record("policy", key, heldAt(time.Minute, HoldDeployment))
		must.Eq(t, base.Add(time.Minute), book.get("policy").HoldSince)

		// So is the same reason after a break.
		book.record("policy", key, &Evaluation{Time: base.Add(2 * time.Minute), Outcome: OutcomeNone})
		book.record("policy", key, heldAt(3*time.Minute, HoldDeployment))
		must.Eq(t, base.Add(3*time.Minute), book.get("policy").HoldSince)
	})

	t.Run("a hold that lasts is told once, and once more when it ends", func(t *testing.T) {
		rec := &eventRecorder{}
		book := newStatusBook(time.Minute, rec.publish)

		book.record("policy", key, heldAt(0, HoldSettling))
		book.record("policy", key, heldAt(50*time.Second, HoldSettling))
		must.Len(t, 0, rec.list())

		book.record("policy", key, heldAt(time.Minute, HoldSettling))
		book.record("policy", key, heldAt(2*time.Minute, HoldSettling))
		events := rec.list()
		must.Len(t, 1, events)
		must.Eq(t, structs.TypeScalingHeld, events[0].typ)
		must.Eq(t, "shop", events[0].ev.JobID)
		must.Eq(t, "policy", events[0].ev.PolicyID)
		must.Eq(t, int64(6), events[0].ev.Count)
		must.StrHasPrefix(t, HoldSettling+": held since ", events[0].ev.Reason)

		book.record("policy", key, &Evaluation{Time: base.Add(3 * time.Minute), Outcome: OutcomeScaled})
		events = rec.list()
		must.Len(t, 2, events)
		must.StrContains(t, events[1].ev.Reason, "ended")
	})

	t.Run("a short hold is not told, nor its end", func(t *testing.T) {
		rec := &eventRecorder{}
		book := newStatusBook(time.Minute, rec.publish)

		book.record("policy", key, heldAt(0, HoldCooldown))
		book.record("policy", key, heldAt(30*time.Second, HoldCooldown))
		book.record("policy", key, &Evaluation{Time: base.Add(40 * time.Second), Outcome: OutcomeNone})
		must.Len(t, 0, rec.list())
	})
}

func TestStatusBook_Forget(t *testing.T) {
	ci.Parallel(t)

	base := time.Unix(1_700_000_000, 0)
	key := targetKey{namespace: "default", job: "shop", group: "web"}
	rec := &eventRecorder{}
	book := newStatusBook(time.Minute, rec.publish)

	eval := &Evaluation{Time: base}
	eval.held(HoldSettling, "")
	book.record("policy", key, eval)
	book.forget("policy")
	must.Nil(t, book.get("policy"))

	// The hold of a forgotten policy starts again.
	again := &Evaluation{Time: base.Add(2 * time.Minute)}
	again.held(HoldSettling, "")
	book.record("policy", key, again)
	must.Eq(t, base.Add(2*time.Minute), book.get("policy").HoldSince)
	must.Len(t, 0, rec.list())
}

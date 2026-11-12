// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"
	"time"

	"github.com/hashicorp/raft"
	autopilot "github.com/hashicorp/raft-autopilot"
	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
)

// The rules of the role promoter that the review of 2026-11-12 found
// missing or wrong (clean room, design raft-observers-and-hot-standby.md).

var (
	ruleLeader  = standbyServer{id: "a", state: autopilot.RaftLeader, role: "voter", index: 100}
	ruleVoter   = standbyServer{id: "b", state: autopilot.RaftVoter, role: "voter", index: 100}
	ruleStandby = standbyServer{id: "s1", state: autopilot.RaftNonVoter, role: "standby", index: 100}
)

func rulePromoter(now *time.Time) *rolePromoter {
	p := newRolePromoter(nil)
	p.now = func() time.Time { return *now }
	return p
}

// A manual failover does not wait for the failover time: a person has
// decided that the voter is lost. Autopilot alone still waits.
func TestRolePromoter_ManualSkipsFailoverTime(t *testing.T) {
	ci.Parallel(t)

	now := testPromoterNow
	state := standbyState("a", ruleLeader, ruleVoter, ruleStandby,
		standbyServer{id: "c", state: autopilot.RaftVoter, role: "voter", index: 90, unhealthyFor: 10 * time.Second})

	p := rulePromoter(&now)
	changes := p.CalculatePromotionsAndDemotions(standbyConf(3), state)
	must.SliceEmpty(t, changes.Promotions)

	p.requestManualPromotion("s1")
	changes = p.CalculatePromotionsAndDemotions(standbyConf(3), state)
	must.Eq(t, []raft.ServerID{"s1"}, changes.Promotions)

	// Withdrawn (the command gave up): autopilot waits again.
	p.clearManualPromotion()
	changes = p.CalculatePromotionsAndDemotions(standbyConf(3), state)
	must.SliceEmpty(t, changes.Promotions)
}

// The cooldown counts from the change as it shows in the state. A promotion
// that was asked for and not applied is asked for again in the next round;
// once it shows, the cooldown holds the next change.
func TestRolePromoter_CooldownFromTheObservedChange(t *testing.T) {
	ci.Parallel(t)

	now := testPromoterNow
	p := rulePromoter(&now)
	conf := standbyConf(3)
	before := standbyState("a", ruleLeader, ruleVoter, ruleStandby,
		standbyServer{id: "s2", state: autopilot.RaftNonVoter, role: "standby", index: 90})
	p.observe(before)

	changes := p.CalculatePromotionsAndDemotions(conf, before)
	must.Eq(t, []raft.ServerID{"s1"}, changes.Promotions)

	// Raft did not apply it: the same state ten seconds later.
	now = now.Add(10 * time.Second)
	p.observe(before)
	changes = p.CalculatePromotionsAndDemotions(conf, before)
	must.Eq(t, []raft.ServerID{"s1"}, changes.Promotions)

	// Applied: the state shows s1 with a vote, and the cooldown starts now.
	promoted := ruleStandby
	promoted.state = autopilot.RaftVoter
	after := standbyState("a", ruleLeader, promoted,
		standbyServer{id: "s2", state: autopilot.RaftNonVoter, role: "standby", index: 90})
	p.observe(after)
	must.Eq(t, now, p.lastStandbyChange)

	// b is gone a minute later: two voters again, and the cooldown holds s2.
	now = now.Add(time.Minute)
	changes = p.CalculatePromotionsAndDemotions(conf, after)
	must.SliceEmpty(t, changes.Promotions)

	// After the cooldown s2 is promoted.
	now = now.Add(5 * time.Minute)
	changes = p.CalculatePromotionsAndDemotions(conf, after)
	must.Eq(t, []raft.ServerID{"s2"}, changes.Promotions)
}

// A manual request lasts until its promotion shows in the state: a round in
// which Raft did not apply it does not use the request up.
func TestRolePromoter_ManualUntilObserved(t *testing.T) {
	ci.Parallel(t)

	now := testPromoterNow
	p := rulePromoter(&now)
	conf := standbyConf(3)
	p.lastStandbyChange = now.Add(-time.Minute) // inside the cooldown
	before := standbyState("a", ruleLeader, ruleVoter, ruleStandby,
		standbyServer{id: "s2", state: autopilot.RaftNonVoter, role: "standby", index: 100})
	p.observe(before)

	must.SliceEmpty(t, p.CalculatePromotionsAndDemotions(conf, before).Promotions)

	p.requestManualPromotion("s2")
	for range 2 {
		must.Eq(t, []raft.ServerID{"s2"}, p.CalculatePromotionsAndDemotions(conf, before).Promotions)
	}

	promoted := standbyServer{id: "s2", state: autopilot.RaftVoter, role: "standby", index: 100}
	after := standbyState("a", ruleLeader, ruleVoter, ruleStandby, promoted)
	p.observe(after)
	must.False(t, p.manual)
	must.Eq(t, raft.ServerID(""), p.preferred)
}

// A voter whose role no longer votes is demoted only while the voters that
// are left keep a healthy majority among themselves.
func TestRolePromoter_RoleDemotionKeepsQuorum(t *testing.T) {
	ci.Parallel(t)

	observer := func(id string) standbyServer {
		return standbyServer{id: id, state: autopilot.RaftVoter, role: "observer", index: 100}
	}
	down := func(id string) standbyServer {
		return standbyServer{id: id, state: autopilot.RaftVoter, role: "voter", index: 90, unhealthyFor: 10 * time.Minute}
	}

	cases := []struct {
		name      string
		state     *autopilot.State
		demotions []raft.ServerID
	}{
		{
			name:      "three healthy voters: the observer is demoted",
			state:     standbyState("a", ruleLeader, ruleVoter, observer("c")),
			demotions: []raft.ServerID{"c"},
		},
		{
			name:  "another voter is down: one healthy voter of two would be left",
			state: standbyState("a", ruleLeader, down("b"), observer("c")),
		},
		{
			name:      "five voters, one down: four are left, three of them healthy",
			state:     standbyState("a", ruleLeader, ruleVoter, down("d"), standbyServer{id: "e", state: autopilot.RaftVoter, role: "voter", index: 100}, observer("c")),
			demotions: []raft.ServerID{"c"},
		},
		{
			name:      "two voters: the voter-role one is left alone, and is its own majority",
			state:     standbyState("a", ruleLeader, observer("c")),
			demotions: []raft.ServerID{"c"},
		},
		{
			name: "the observer itself is the one that is down: the others are healthy",
			state: standbyState("a", ruleLeader, ruleVoter,
				standbyServer{id: "c", state: autopilot.RaftVoter, role: "observer", index: 90, unhealthyFor: 10 * time.Minute}),
			demotions: []raft.ServerID{"c"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := testPromoterNow
			p := rulePromoter(&now)
			changes := p.CalculatePromotionsAndDemotions(standbyConf(0), tc.state)
			must.Eq(t, tc.demotions, changes.Demotions)
			must.SliceEmpty(t, changes.Promotions)
		})
	}
}

// With a target the dead server cleanup leaves a failed voter in the
// configuration until it has been unhealthy for the failover time.
func TestRolePromoter_FilterFailedServerRemovals(t *testing.T) {
	ci.Parallel(t)

	state := standbyState("a", ruleLeader, ruleStandby,
		standbyServer{id: "new", state: autopilot.RaftVoter, role: "voter", unhealthyFor: 10 * time.Second},
		standbyServer{id: "old", state: autopilot.RaftVoter, role: "voter", unhealthyFor: 2 * time.Minute},
		standbyServer{id: "obs", state: autopilot.RaftNonVoter, role: "observer", unhealthyFor: time.Second})
	server := func(id string) *autopilot.Server { return &state.Servers[raft.ServerID(id)].Server }
	failed := func() *autopilot.FailedServers {
		return &autopilot.FailedServers{
			StaleNonVoters:  []raft.ServerID{"gone-nonvoter"},
			StaleVoters:     []raft.ServerID{"new", "old", "gone-voter"},
			FailedNonVoters: []*autopilot.Server{server("obs")},
			FailedVoters:    []*autopilot.Server{server("new"), server("old")},
		}
	}
	now := testPromoterNow
	p := rulePromoter(&now)

	t.Run("without a target nothing is held", func(t *testing.T) {
		must.Eq(t, failed(), p.FilterFailedServerRemovals(standbyConf(0), state, failed()))
		must.Eq(t, failed(), p.FilterFailedServerRemovals(&autopilot.Config{}, state, failed()))
	})

	t.Run("with a target a voter waits for the failover time", func(t *testing.T) {
		in := failed()
		out := p.FilterFailedServerRemovals(standbyConf(3), state, in)
		must.Eq(t, []raft.ServerID{"gone-nonvoter"}, out.StaleNonVoters)
		must.Eq(t, []*autopilot.Server{server("obs")}, out.FailedNonVoters)
		must.Eq(t, []raft.ServerID{"old", "gone-voter"}, out.StaleVoters)
		must.Eq(t, []*autopilot.Server{server("old")}, out.FailedVoters)
		// What autopilot handed in is not changed.
		must.Eq(t, failed(), in)
	})

	t.Run("after the failover time it is removed", func(t *testing.T) {
		now = testPromoterNow.Add(time.Minute)
		defer func() { now = testPromoterNow }()
		out := p.FilterFailedServerRemovals(standbyConf(3), state, failed())
		must.Eq(t, failed().FailedVoters, out.FailedVoters)
		must.Eq(t, failed().StaleVoters, out.StaleVoters)
	})

	t.Run("nothing to filter", func(t *testing.T) {
		must.Nil(t, p.FilterFailedServerRemovals(standbyConf(3), state, nil))
	})
}

// A manual failover removes nothing unless autopilot would promote a standby
// afterwards.
func TestManualPromotionBlocked(t *testing.T) {
	ci.Parallel(t)

	voter := func(id string) standbyServer {
		return standbyServer{id: id, state: autopilot.RaftVoter, role: "voter", index: 100}
	}
	down := standbyServer{id: "c", state: autopilot.RaftVoter, role: "voter", index: 90, unhealthyFor: time.Minute}

	cases := []struct {
		name     string
		state    *autopilot.State
		replace  string
		expected string
	}{
		{
			name:  "a voter of three is down: the standby would be promoted",
			state: standbyState("a", ruleLeader, ruleVoter, down, ruleStandby),
		},
		{
			name:    "the same, with the failed voter replaced",
			state:   standbyState("a", ruleLeader, ruleVoter, down, ruleStandby),
			replace: "c",
		},
		{
			name:     "all voters healthy, nothing to replace",
			state:    standbyState("a", ruleLeader, ruleVoter, voter("c"), ruleStandby),
			expected: "name the failed voter with -replace",
		},
		{
			name:    "a healthy voter replaced by force: two are left of a target of three",
			state:   standbyState("a", ruleLeader, ruleVoter, voter("c"), ruleStandby),
			replace: "c",
		},
		{
			name:     "five voters, one down: without it four still meet the target",
			state:    standbyState("a", ruleLeader, ruleVoter, down, voter("d"), voter("e"), ruleStandby),
			replace:  "c",
			expected: `4 healthy voters without "c" still meet target_voters 3`,
		},
		{
			name: "a voter-role server is about to get its vote",
			state: standbyState("a", ruleLeader, ruleVoter, down, ruleStandby,
				standbyServer{id: "n", state: autopilot.RaftNonVoter, role: "voter", index: 100, fresh: true}),
			replace:  "c",
			expected: `server "n" has the role voter and is about to get its vote`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := manualPromotionBlocked(tc.state, 3, raft.ServerID(tc.replace))
			if tc.expected == "" {
				must.Eq(t, "", got)
				return
			}
			must.StrContains(t, got, tc.expected)
		})
	}
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/hashicorp/raft"
	autopilot "github.com/hashicorp/raft-autopilot"
	"github.com/shoenig/test/must"
)

// promoterServer builds the autopilot state of one server for the promoter
// tests: its raft state, its role tag (empty: no tag) and whether it has been
// healthy for a minute.
func promoterServer(id string, state autopilot.RaftState, role string, stable bool) *autopilot.ServerState {
	meta := map[string]string{"role": "nomad"}
	if role != "" {
		meta[serverRoleTag] = role
	}
	srv := &autopilot.ServerState{
		Server: autopilot.Server{ID: raft.ServerID(id), Name: id, Meta: meta},
		State:  state,
	}
	if stable {
		srv.Health = autopilot.ServerHealth{Healthy: true, StableSince: testPromoterNow.Add(-time.Minute)}
	} else {
		srv.Health = autopilot.ServerHealth{Healthy: true, StableSince: testPromoterNow}
	}
	return srv
}

var testPromoterNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

func promoterState(leader string, servers ...*autopilot.ServerState) *autopilot.State {
	s := &autopilot.State{Leader: raft.ServerID(leader), Servers: map[raft.ServerID]*autopilot.ServerState{}}
	for _, srv := range servers {
		s.Servers[srv.Server.ID] = srv
	}
	return s
}

func TestRolePromoter_CalculatePromotionsAndDemotions(t *testing.T) {
	ci.Parallel(t)

	conf := &autopilot.Config{ServerStabilizationTime: 10 * time.Second}
	p := &rolePromoter{now: func() time.Time { return testPromoterNow }}

	cases := []struct {
		name       string
		state      *autopilot.State
		promotions []raft.ServerID
		demotions  []raft.ServerID
		leader     raft.ServerID
	}{
		{
			name: "stable voter-role non-voter is promoted, also without a tag",
			state: promoterState("a",
				promoterServer("a", autopilot.RaftLeader, "", true),
				promoterServer("b", autopilot.RaftNonVoter, "voter", true),
				promoterServer("c", autopilot.RaftNonVoter, "", true)),
			promotions: []raft.ServerID{"b", "c"},
		},
		{
			name: "unstable voter-role non-voter waits",
			state: promoterState("a",
				promoterServer("a", autopilot.RaftLeader, "voter", true),
				promoterServer("b", autopilot.RaftNonVoter, "voter", false)),
		},
		{
			name: "observers and standbys are never promoted",
			state: promoterState("a",
				promoterServer("a", autopilot.RaftLeader, "voter", true),
				promoterServer("b", autopilot.RaftNonVoter, "observer", true),
				promoterServer("c", autopilot.RaftNonVoter, "standby", true)),
		},
		{
			name: "an unknown role votes",
			state: promoterState("a",
				promoterServer("a", autopilot.RaftLeader, "voter", true),
				promoterServer("b", autopilot.RaftNonVoter, "witness", true)),
			promotions: []raft.ServerID{"b"},
		},
		{
			name: "a voter that is now an observer is demoted, one per round",
			state: promoterState("a",
				promoterServer("a", autopilot.RaftLeader, "voter", true),
				promoterServer("b", autopilot.RaftVoter, "observer", true),
				promoterServer("c", autopilot.RaftVoter, "standby", true)),
			demotions: []raft.ServerID{"b"},
		},
		{
			name: "promotions first, demotions in a later round",
			state: promoterState("a",
				promoterServer("a", autopilot.RaftLeader, "voter", true),
				promoterServer("b", autopilot.RaftVoter, "observer", true),
				promoterServer("c", autopilot.RaftNonVoter, "voter", true)),
			promotions: []raft.ServerID{"c"},
		},
		{
			name: "a leader that is now an observer hands over the leadership first",
			state: promoterState("a",
				promoterServer("a", autopilot.RaftLeader, "observer", true),
				promoterServer("b", autopilot.RaftVoter, "voter", true),
				promoterServer("c", autopilot.RaftVoter, "voter", true)),
			leader: "b",
		},
		{
			name: "no demotion when no voter-role server keeps the vote",
			state: promoterState("a",
				promoterServer("a", autopilot.RaftLeader, "observer", true),
				promoterServer("b", autopilot.RaftVoter, "observer", true)),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changes := p.CalculatePromotionsAndDemotions(conf, tc.state)
			must.Eq(t, tc.promotions, changes.Promotions)
			must.Eq(t, tc.demotions, changes.Demotions)
			must.Eq(t, tc.leader, changes.Leader)
		})
	}
}

func TestRolePromoter_GetNodeTypes(t *testing.T) {
	ci.Parallel(t)

	p := newRolePromoter()
	types := p.GetNodeTypes(&autopilot.Config{}, promoterState("a",
		promoterServer("a", autopilot.RaftLeader, "", true),
		promoterServer("b", autopilot.RaftNonVoter, "observer", true),
		promoterServer("c", autopilot.RaftNonVoter, "standby", true)))
	must.Eq(t, map[raft.ServerID]autopilot.NodeType{
		"a": "voter", "b": "observer", "c": "standby",
	}, types)
}

// TestAutopilot_ObserverNeverPromoted joins an observer to a running server:
// it stays a non-voter, reports its role, and a voter-role server that joins
// after it is promoted as before.
func TestAutopilot_ObserverNeverPromoted(t *testing.T) {
	ci.Parallel(t)

	s1, cleanupS1 := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
	})
	defer cleanupS1()
	testutil.WaitForLeader(t, s1.RPC)

	s2, cleanupS2 := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
		c.BootstrapExpect = 0
		c.Role = ServerRoleObserver
	})
	defer cleanupS2()

	s3, cleanupS3 := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
		c.BootstrapExpect = 0
	})
	defer cleanupS3()
	TestJoin(t, s1, s2, s3)

	suffrage := func() (map[raft.ServerID]raft.ServerSuffrage, error) {
		future := s1.raft.GetConfiguration()
		if err := future.Error(); err != nil {
			return nil, err
		}
		out := map[raft.ServerID]raft.ServerSuffrage{}
		for _, srv := range future.Configuration().Servers {
			out[srv.ID] = srv.Suffrage
		}
		return out, nil
	}

	// The voter-role server is promoted; the observer is in the
	// configuration as a non-voter.
	testutil.WaitForResultUntil(20*time.Second, func() (bool, error) {
		got, err := suffrage()
		if err != nil {
			return false, err
		}
		if got[raft.ServerID(s3.config.NodeID)] != raft.Voter {
			return false, fmt.Errorf("s3 not a voter yet: %v", got)
		}
		if s, ok := got[raft.ServerID(s2.config.NodeID)]; !ok || s != raft.Nonvoter {
			return false, fmt.Errorf("s2 should be a non-voter: %v", got)
		}
		return true, nil
	}, func(err error) { must.NoError(t, err) })

	// It stays a non-voter well past the stabilization time.
	stabilization := s1.config.AutopilotConfig.ServerStabilizationTime
	time.Sleep(stabilization + 2*time.Second)
	got, err := suffrage()
	must.NoError(t, err)
	must.Eq(t, raft.Nonvoter, got[raft.ServerID(s2.config.NodeID)])

	// The health and the raft configuration report the roles.
	testutil.WaitForResultUntil(10*time.Second, func() (bool, error) {
		health := s1.GetClusterHealth()
		if health == nil {
			return false, fmt.Errorf("no health yet")
		}
		roles := map[string]string{}
		for _, srv := range health.Servers {
			roles[srv.ID] = srv.Role
		}
		if roles[s2.config.NodeID] != "observer" || roles[s3.config.NodeID] != "voter" {
			return false, fmt.Errorf("roles: %v", roles)
		}
		return true, nil
	}, func(err error) { must.NoError(t, err) })
}

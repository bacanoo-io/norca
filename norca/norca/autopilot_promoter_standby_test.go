// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/stream"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/raft"
	autopilot "github.com/hashicorp/raft-autopilot"
	"github.com/shoenig/test/must"
)

// standbyServer builds the autopilot state of one server for the standby
// tests.
type standbyServer struct {
	id    string
	state autopilot.RaftState
	role  string
	dc    string
	index uint64
	// unhealthyFor > 0: the server has been unhealthy for that long;
	// otherwise healthy for a minute (stable).
	unhealthyFor time.Duration
	// fresh: healthy, but not stable yet.
	fresh bool
}

func (b standbyServer) build() *autopilot.ServerState {
	meta := map[string]string{"role": "nomad", serverRoleTag: b.role, "dc": b.dc}
	if b.dc == "" {
		meta["dc"] = "dc1"
	}
	srv := &autopilot.ServerState{
		Server: autopilot.Server{ID: raft.ServerID(b.id), Name: b.id + ".global", Meta: meta},
		State:  b.state,
		Stats:  autopilot.ServerStats{LastIndex: b.index},
	}
	switch {
	case b.unhealthyFor > 0:
		srv.Health = autopilot.ServerHealth{Healthy: false, StableSince: testPromoterNow.Add(-b.unhealthyFor)}
	case b.fresh:
		srv.Health = autopilot.ServerHealth{Healthy: true, StableSince: testPromoterNow}
	default:
		srv.Health = autopilot.ServerHealth{Healthy: true, StableSince: testPromoterNow.Add(-time.Minute)}
	}
	return srv
}

func standbyState(leader string, servers ...standbyServer) *autopilot.State {
	s := &autopilot.State{Leader: raft.ServerID(leader), Servers: map[raft.ServerID]*autopilot.ServerState{}}
	for _, b := range servers {
		srv := b.build()
		s.Servers[srv.Server.ID] = srv
	}
	return s
}

func standbyConf(target int) *autopilot.Config {
	return &autopilot.Config{
		ServerStabilizationTime: 10 * time.Second,
		Ext: &standbyConfig{
			targetVoters:  target,
			failoverAfter: time.Minute,
			cooldown:      5 * time.Minute,
		},
	}
}

func TestRolePromoter_Standby(t *testing.T) {
	ci.Parallel(t)

	leader := standbyServer{id: "a", state: autopilot.RaftLeader, role: "voter", index: 100}
	voter := func(id string) standbyServer {
		return standbyServer{id: id, state: autopilot.RaftVoter, role: "voter", index: 100}
	}
	standby := func(id string, index uint64) standbyServer {
		return standbyServer{id: id, state: autopilot.RaftNonVoter, role: "standby", index: index}
	}

	cases := []struct {
		name string
		// target_voters; the configuration has no Ext when it is -1.
		target int
		// lastChange: the last standby change was this long ago (0: never).
		lastChange time.Duration
		state      *autopilot.State
		promotions []raft.ServerID
		demotions  []raft.ServerID
		leader     raft.ServerID
	}{
		{
			name:   "no target: a standby is never promoted",
			target: 0,
			state: standbyState("a", leader, voter("b"),
				standbyServer{id: "c", state: autopilot.RaftVoter, role: "voter", unhealthyFor: time.Hour},
				standby("s", 100)),
		},
		{
			name:   "no configuration extension: as no target",
			target: -1,
			state: standbyState("a", leader, voter("b"),
				standbyServer{id: "c", state: autopilot.RaftVoter, role: "voter", unhealthyFor: time.Hour},
				standby("s", 100)),
		},
		{
			name:   "no target: a promoted standby is demoted",
			target: 0,
			state: standbyState("a", leader, voter("b"),
				standbyServer{id: "s", state: autopilot.RaftVoter, role: "standby", index: 100}),
			demotions: []raft.ServerID{"s"},
		},
		{
			name:   "a voter unhealthy for the failover time: the standby is promoted",
			target: 3,
			state: standbyState("a", leader, voter("b"),
				standbyServer{id: "c", state: autopilot.RaftVoter, role: "voter", unhealthyFor: 2 * time.Minute},
				standby("s", 100)),
			promotions: []raft.ServerID{"s"},
		},
		{
			name:   "a voter unhealthy for less than the failover time: nothing changes",
			target: 3,
			state: standbyState("a", leader, voter("b"),
				standbyServer{id: "c", state: autopilot.RaftVoter, role: "voter", unhealthyFor: 30 * time.Second},
				standby("s", 100)),
		},
		{
			name:       "a voter missing from the configuration: the standby is promoted",
			target:     3,
			state:      standbyState("a", leader, voter("b"), standby("s", 100)),
			promotions: []raft.ServerID{"s"},
		},
		{
			name:   "the target is met: nothing changes",
			target: 3,
			state:  standbyState("a", leader, voter("b"), voter("c"), standby("s", 100)),
		},
		{
			name:       "the cooldown holds a promotion",
			target:     3,
			lastChange: time.Minute,
			state:      standbyState("a", leader, voter("b"), standby("s", 100)),
		},
		{
			name:       "after the cooldown the standby is promoted",
			target:     3,
			lastChange: 6 * time.Minute,
			state:      standbyState("a", leader, voter("b"), standby("s", 100)),
			promotions: []raft.ServerID{"s"},
		},
		{
			name:   "a voter-role server about to be promoted: no standby",
			target: 3,
			state: standbyState("a", leader, voter("b"), standby("s", 100),
				standbyServer{id: "v", state: autopilot.RaftNonVoter, role: "voter", fresh: true}),
		},
		{
			name:   "a stable voter-role non-voter goes first",
			target: 3,
			state: standbyState("a", leader, voter("b"), standby("s", 100),
				standbyServer{id: "v", state: autopilot.RaftNonVoter, role: "voter"}),
			promotions: []raft.ServerID{"v"},
		},
		{
			name:   "an unhealthy or unstable standby is not chosen",
			target: 3,
			state: standbyState("a", leader, voter("b"),
				standbyServer{id: "s1", state: autopilot.RaftNonVoter, role: "standby", unhealthyFor: time.Minute},
				standbyServer{id: "s2", state: autopilot.RaftNonVoter, role: "standby", fresh: true}),
		},
		{
			name:       "the most caught-up standby wins",
			target:     3,
			state:      standbyState("a", leader, voter("b"), standby("s1", 90), standby("s2", 99), standby("s3", 95)),
			promotions: []raft.ServerID{"s2"},
		},
		{
			name:       "equal standbys: the lowest ID wins",
			target:     3,
			state:      standbyState("a", leader, voter("b"), standby("s2", 99), standby("s1", 99)),
			promotions: []raft.ServerID{"s1"},
		},
		{
			name:   "the datacenter of the failed voter goes before the index",
			target: 3,
			state: standbyState("a", leader, voter("b"),
				standbyServer{id: "c", state: autopilot.RaftVoter, role: "voter", dc: "dc2", unhealthyFor: 2 * time.Minute},
				standbyServer{id: "s1", state: autopilot.RaftNonVoter, role: "standby", dc: "dc1", index: 100},
				standbyServer{id: "s2", state: autopilot.RaftNonVoter, role: "standby", dc: "dc2", index: 90}),
			promotions: []raft.ServerID{"s2"},
		},
		{
			name:   "observers are never promoted for a missing voter",
			target: 3,
			state: standbyState("a", leader, voter("b"),
				standbyServer{id: "o", state: autopilot.RaftNonVoter, role: "observer", index: 100}),
		},
		{
			name:   "a returned voter: the promoted standby steps back",
			target: 3,
			state: standbyState("a", leader, voter("b"), voter("c"),
				standbyServer{id: "s", state: autopilot.RaftVoter, role: "standby", index: 100}),
			demotions: []raft.ServerID{"s"},
		},
		{
			name:       "a returned voter: the demotion waits for the cooldown",
			target:     3,
			lastChange: time.Minute,
			state: standbyState("a", leader, voter("b"), voter("c"),
				standbyServer{id: "s", state: autopilot.RaftVoter, role: "standby", index: 100}),
		},
		{
			name:   "no demotion that leaves fewer healthy voters than the target",
			target: 3,
			state: standbyState("a", leader, voter("b"),
				standbyServer{id: "c", state: autopilot.RaftVoter, role: "voter", unhealthyFor: 10 * time.Second},
				standbyServer{id: "s", state: autopilot.RaftVoter, role: "standby", index: 100}),
		},
		{
			name:   "two promoted standbys: the least caught-up steps back",
			target: 3,
			state: standbyState("a", leader, voter("b"), voter("c"),
				standbyServer{id: "s1", state: autopilot.RaftVoter, role: "standby", index: 100},
				standbyServer{id: "s2", state: autopilot.RaftVoter, role: "standby", index: 80}),
			demotions: []raft.ServerID{"s2"},
		},
		{
			name:   "a promoted standby that leads hands over first",
			target: 3,
			state: standbyState("s", voter("a"), voter("b"), voter("c"),
				standbyServer{id: "s", state: autopilot.RaftLeader, role: "standby", index: 100}),
			leader: "a",
		},
		{
			name:   "voters at the target with a promoted standby: it stays",
			target: 3,
			state: standbyState("a", leader, voter("b"),
				standbyServer{id: "s", state: autopilot.RaftVoter, role: "standby", index: 100}),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newRolePromoter(nil)
			p.now = func() time.Time { return testPromoterNow }
			if tc.lastChange > 0 {
				p.lastStandbyChange = testPromoterNow.Add(-tc.lastChange)
			}
			conf := standbyConf(tc.target)
			if tc.target < 0 {
				conf.Ext = nil
			}
			changes := p.CalculatePromotionsAndDemotions(conf, tc.state)
			must.Eq(t, tc.promotions, changes.Promotions)
			must.Eq(t, tc.demotions, changes.Demotions)
			must.Eq(t, tc.leader, changes.Leader)
		})
	}
}

// TestRolePromoter_StandbyCooldownAndManual checks that a promotion that
// shows in the state starts the cooldown, and that a manual request skips it
// and chooses its standby.
func TestRolePromoter_StandbyCooldownAndManual(t *testing.T) {
	ci.Parallel(t)

	now := testPromoterNow
	p := newRolePromoter(nil)
	p.now = func() time.Time { return now }
	conf := standbyConf(3)

	leader := standbyServer{id: "a", state: autopilot.RaftLeader, role: "voter", index: 100}
	voter := standbyServer{id: "b", state: autopilot.RaftVoter, role: "voter", index: 100}
	s1 := standbyServer{id: "s1", state: autopilot.RaftNonVoter, role: "standby", index: 100}
	s2 := standbyServer{id: "s2", state: autopilot.RaftNonVoter, role: "standby", index: 90}
	state := standbyState("a", leader, voter, s1, s2)
	p.observe(state)

	changes := p.CalculatePromotionsAndDemotions(conf, state)
	must.Eq(t, []raft.ServerID{"s1"}, changes.Promotions)

	// The promotion shows in the state: the cooldown starts.
	s1.state = autopilot.RaftVoter
	p.observe(standbyState("a", leader, voter, s1, s2))

	// Say s1 was lost again right away: the cooldown holds the next one.
	now = now.Add(time.Minute)
	state = standbyState("a", leader, voter, s2)
	p.observe(state)
	changes = p.CalculatePromotionsAndDemotions(conf, state)
	must.SliceEmpty(t, changes.Promotions)

	// A manual request skips the cooldown and chooses its standby.
	p.requestManualPromotion("s2")
	changes = p.CalculatePromotionsAndDemotions(conf, state)
	must.Eq(t, []raft.ServerID{"s2"}, changes.Promotions)

	// Its promotion shows: the request is used up, and the cooldown applies
	// again.
	s2.state = autopilot.RaftVoter
	state = standbyState("a", leader, s2,
		standbyServer{id: "s3", state: autopilot.RaftNonVoter, role: "standby", index: 100})
	p.observe(state)
	changes = p.CalculatePromotionsAndDemotions(conf, state)
	must.SliceEmpty(t, changes.Promotions)
}

// TestRolePromoter_Observe checks the events of the standby changes: none for
// the first state of a leadership, one per change after it, with the reason
// of the promoter.
func TestRolePromoter_Observe(t *testing.T) {
	ci.Parallel(t)

	p := newRolePromoter(nil)
	p.now = func() time.Time { return testPromoterNow }
	var events []*structs.ServerRoleEvent
	p.onChange = func(ev *structs.ServerRoleEvent) { events = append(events, ev) }

	leader := standbyServer{id: "a", state: autopilot.RaftLeader, role: "voter", index: 100}
	voter := standbyServer{id: "b", state: autopilot.RaftVoter, role: "voter", index: 100}
	failed := standbyServer{id: "c", state: autopilot.RaftVoter, role: "voter", unhealthyFor: 2 * time.Minute}
	before := standbyState("a", leader, voter, failed,
		standbyServer{id: "s", state: autopilot.RaftNonVoter, role: "standby", index: 100})

	p.observe(before)
	must.SliceEmpty(t, events)

	changes := p.CalculatePromotionsAndDemotions(standbyConf(3), before)
	must.Eq(t, []raft.ServerID{"s"}, changes.Promotions)

	after := standbyState("a", leader, voter, failed,
		standbyServer{id: "s", state: autopilot.RaftVoter, role: "standby", index: 100})
	p.observe(after)
	must.Len(t, 1, events)
	must.Eq(t, structs.TypeStandbyPromoted, events[0].Type)
	must.Eq(t, "s", events[0].ID)
	must.True(t, events[0].Voter)
	must.Eq(t, "c", events[0].Replaced)
	must.StrContains(t, events[0].Reason, "voter c unhealthy since")

	promotions, demotions := p.standbyCounts()
	must.Eq(t, uint64(1), promotions)
	must.Eq(t, uint64(0), demotions)

	// No change, no event.
	p.observe(after)
	must.Len(t, 1, events)

	// A new leadership starts with a new baseline.
	p.reset()
	p.observe(before)
	must.Len(t, 1, events)
}

func TestAutopilotConfig_ValidateStandby(t *testing.T) {
	ci.Parallel(t)

	for _, n := range []uint{0, 1, 3, 5, 7} {
		must.NoError(t, (&structs.AutopilotConfig{TargetVoters: n}).ValidateStandby())
	}
	for _, n := range []uint{2, 4, 6, 8, 9} {
		must.Error(t, (&structs.AutopilotConfig{TargetVoters: n}).ValidateStandby())
	}
	must.Error(t, (&structs.AutopilotConfig{StandbyFailoverAfter: -time.Second}).ValidateStandby())
	must.Error(t, (&structs.AutopilotConfig{StandbyCooldown: -time.Second}).ValidateStandby())

	c := &structs.AutopilotConfig{}
	must.Eq(t, structs.DefaultStandbyFailoverAfter, c.StandbyFailoverAfterOrDefault())
	must.Eq(t, structs.DefaultStandbyCooldown, c.StandbyCooldownOrDefault())
}

// standbyTestCluster starts three voters and one standby with target_voters
// 3 and returns them; the standby joins after the voters have their votes.
func standbyTestCluster(t *testing.T, apConf func(*structs.AutopilotConfig)) (servers []*Server, standby *Server) {
	t.Helper()

	s1, cleanup1 := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
		c.AutopilotConfig.TargetVoters = 3
		c.AutopilotConfig.StandbyFailoverAfter = time.Second
		c.AutopilotConfig.StandbyCooldown = time.Second
		if apConf != nil {
			apConf(c.AutopilotConfig)
		}
	})
	t.Cleanup(cleanup1)
	testutil.WaitForLeader(t, s1.RPC)

	s2, cleanup2 := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
		c.BootstrapExpect = 0
	})
	t.Cleanup(cleanup2)
	s3, cleanup3 := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
		c.BootstrapExpect = 0
	})
	t.Cleanup(cleanup3)
	TestJoin(t, s1, s2, s3)
	waitForSuffrage(t, s1, map[*Server]raft.ServerSuffrage{s1: raft.Voter, s2: raft.Voter, s3: raft.Voter})

	s4, cleanup4 := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
		c.BootstrapExpect = 0
		c.Role = ServerRoleStandby
	})
	t.Cleanup(cleanup4)
	TestJoin(t, s1, s4)
	waitForSuffrage(t, s1, map[*Server]raft.ServerSuffrage{s4: raft.Nonvoter})

	// Autopilot's state follows the Raft configuration a few rounds later:
	// wait until it has the standby healthy and stable.
	testutil.WaitForResultUntil(30*time.Second, func() (bool, error) {
		h := s1.autopilot.GetServerHealth(raft.ServerID(s4.config.NodeID))
		if h == nil || !h.IsStable(time.Now(), s1.config.AutopilotConfig.ServerStabilizationTime) {
			return false, fmt.Errorf("standby not stable for autopilot yet: %+v", h)
		}
		return true, nil
	}, func(err error) { must.NoError(t, err) })

	return []*Server{s1, s2, s3}, s4
}

// waitForSuffrage waits until the Raft configuration of the leader shows
// the servers with the suffrage.
func waitForSuffrage(t *testing.T, leader *Server, want map[*Server]raft.ServerSuffrage) {
	t.Helper()
	testutil.WaitForResultUntil(30*time.Second, func() (bool, error) {
		future := leader.raft.GetConfiguration()
		if err := future.Error(); err != nil {
			return false, err
		}
		got := map[raft.ServerID]raft.ServerSuffrage{}
		for _, srv := range future.Configuration().Servers {
			got[srv.ID] = srv.Suffrage
		}
		for s, suffrage := range want {
			if g, ok := got[raft.ServerID(s.config.NodeID)]; !ok || g != suffrage {
				return false, fmt.Errorf("server %s: want %v, configuration %v", s.config.NodeName, suffrage, got)
			}
		}
		return true, nil
	}, func(err error) { must.NoError(t, err) })
}

// waitForLeaderOf waits until one of the servers is the leader and returns
// it.
func waitForLeaderOf(t *testing.T, servers ...*Server) *Server {
	t.Helper()
	var leader *Server
	testutil.WaitForResultUntil(30*time.Second, func() (bool, error) {
		for _, s := range servers {
			if s.IsLeader() {
				leader = s
				return true, nil
			}
		}
		return false, fmt.Errorf("no leader")
	}, func(err error) { must.NoError(t, err) })
	return leader
}

// TestAutopilot_StandbyFailover stops a voter of three: autopilot promotes
// the standby and publishes a Server event; a new voter-role server joins:
// the standby steps back after the cooldown.
func TestAutopilot_StandbyFailover(t *testing.T) {
	ci.Parallel(t)

	voters, standby := standbyTestCluster(t, nil)
	s1, s2, s3 := voters[0], voters[1], voters[2]

	// Subscribe to the Server topic on the leader before the failover.
	broker, err := s1.State().EventBroker()
	must.NoError(t, err)
	sub, err := broker.Subscribe(&stream.SubscribeRequest{
		Topics:    map[structs.Topic][]string{structs.TopicServer: {"*"}},
		Namespace: "*",
	})
	must.NoError(t, err)
	defer sub.Unsubscribe()

	// Stop s3: autopilot replaces it with the standby.
	must.NoError(t, s3.Shutdown())
	waitForSuffrage(t, s1, map[*Server]raft.ServerSuffrage{standby: raft.Voter})

	ev := nextServerEvent(t, sub)
	must.Eq(t, structs.TypeStandbyPromoted, ev.Type)
	must.Eq(t, standby.config.NodeID, ev.ID)

	// A new voter-role server joins: it is promoted, and the standby
	// steps back to keep three voters.
	s5, cleanup5 := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
		c.BootstrapExpect = 0
	})
	defer cleanup5()
	TestJoin(t, s1, s5)
	leader := waitForLeaderOf(t, s1, s2)
	waitForSuffrage(t, leader, map[*Server]raft.ServerSuffrage{
		s5: raft.Voter, standby: raft.Nonvoter,
	})

	ev = nextServerEvent(t, sub)
	must.Eq(t, structs.TypeStandbyDemoted, ev.Type)
	must.Eq(t, standby.config.NodeID, ev.ID)

	testutil.WaitForResultUntil(10*time.Second, func() (bool, error) {
		voters, ready, promotions, demotions := leader.standbyGauges()
		if voters != 3 || ready != 1 || promotions != 1 || demotions != 1 {
			return false, fmt.Errorf("voters %d, ready %d, promotions %d, demotions %d",
				voters, ready, promotions, demotions)
		}
		return true, nil
	}, func(err error) { must.NoError(t, err) })
}

func nextServerEvent(t *testing.T, sub *stream.Subscription) *structs.ServerRoleEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		events, err := sub.Next(ctx)
		must.NoError(t, err)
		for _, e := range events.Events {
			if e.Topic != structs.TopicServer {
				continue
			}
			ev, ok := e.Payload.(*structs.ServerRoleEvent)
			must.True(t, ok)
			return ev
		}
	}
}

// TestOperator_RaftPromoteStandby checks the manual failover: its checks,
// then the removal of a stopped voter and the promotion of the standby
// without the cooldown or the failover time.
func TestOperator_RaftPromoteStandby(t *testing.T) {
	ci.Parallel(t)

	voters, standby := standbyTestCluster(t, func(c *structs.AutopilotConfig) {
		// Autopilot alone would not act during the test.
		c.CleanupDeadServers = false
		c.StandbyFailoverAfter = time.Hour
		c.StandbyCooldown = time.Hour
	})
	s1, s2, s3 := voters[0], voters[1], voters[2]
	codec := rpcClient(t, s1)

	call := func(id, replace string, force bool) (*structs.RaftPromoteStandbyResponse, error) {
		args := &structs.RaftPromoteStandbyRequest{
			ID: raft.ServerID(id), Replace: raft.ServerID(replace), Force: force,
			WriteRequest: structs.WriteRequest{Region: s1.config.Region},
		}
		var reply structs.RaftPromoteStandbyResponse
		err := msgpackrpc.CallWithCodec(codec, "Operator.RaftPromoteStandby", args, &reply)
		return &reply, err
	}

	// The checks.
	_, err := call(s2.config.NodeID, "", false)
	must.ErrorContains(t, err, `not "standby"`)
	_, err = call("no-such-server", "", false)
	must.ErrorContains(t, err, "not in the Raft configuration seen by autopilot")
	_, err = call(standby.config.NodeID, "", false)
	must.ErrorContains(t, err, "name the failed voter with -replace")
	_, err = call(standby.config.NodeID, s2.config.NodeID, false)
	must.ErrorContains(t, err, "use -force")
	_, err = call(standby.config.NodeID, s1.config.NodeID, true)
	must.ErrorContains(t, err, "is the leader")

	// Stop s3 and wait until autopilot sees it unhealthy.
	must.NoError(t, s3.Shutdown())
	testutil.WaitForResultUntil(30*time.Second, func() (bool, error) {
		if h := s1.autopilot.GetServerHealth(raft.ServerID(s3.config.NodeID)); h == nil || h.Healthy {
			return false, fmt.Errorf("s3 still healthy")
		}
		return true, nil
	}, func(err error) { must.NoError(t, err) })

	reply, err := call(standby.config.NodeID, s3.config.NodeID, false)
	must.NoError(t, err)
	got := map[string]bool{}
	for _, srv := range reply.Configuration.Servers {
		got[string(srv.ID)] = srv.Voter
	}
	must.Eq(t, map[string]bool{
		s1.config.NodeID: true, s2.config.NodeID: true, standby.config.NodeID: true,
	}, got)

	// Without a target nothing would promote a standby.
	args := &structs.AutopilotSetConfigRequest{
		Datacenter:   s1.config.Datacenter,
		Config:       *s1.getOrCreateAutopilotConfig(),
		WriteRequest: structs.WriteRequest{Region: s1.config.Region},
	}
	args.Config.TargetVoters = 0
	var ok bool
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Operator.AutopilotSetConfiguration", args, &ok))
	_, err = call(standby.config.NodeID, "", false)
	must.ErrorContains(t, err, "target_voters is 0")

	// An even target is refused.
	args.Config.TargetVoters = 4
	err = msgpackrpc.CallWithCodec(codec, "Operator.AutopilotSetConfiguration", args, &ok)
	must.ErrorContains(t, err, "must be 0, 1, 3, 5 or 7")
}

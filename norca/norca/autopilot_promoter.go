// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	autopilot "github.com/hashicorp/raft-autopilot"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// rolePromoter is the autopilot promoter of Norca (clean room, design
// raft-observers-and-hot-standby.md, phases 1 and 2). It replaces the
// default StablePromoter, which promotes every stable non-voter, with rules
// that follow the role each server advertises in its Serf tags:
//
//   - a stable non-voter with the voter role is promoted (the behaviour of
//     the default promoter for voters);
//   - observers are never promoted;
//   - standbys are promoted only with a target number of voters
//     (autopilot target_voters): when the healthy voters are fewer than the
//     target because a voter has been unhealthy for standby_failover_after,
//     or is missing from the configuration, the most suitable standby is
//     promoted (standbyCandidate);
//   - a promoted standby steps back when the voters exceed the target and
//     the others keep it healthy (a returned voter);
//   - a voter whose role does not vote (a server restarted with another
//     role; a standby when there is no target) is demoted, one per round,
//     and only while a voter-role server keeps the vote;
//   - a leader that must lose its vote hands the leadership to a healthy
//     voter-role voter first.
//
// The standby changes are at most one per round and keep a cooldown
// (standby_cooldown) between them, counted from the change as it shows in
// the state, not from the request for it: a change that was not applied is
// asked for again in the next round.
//
// With a target, the dead server cleanup of autopilot leaves a failed voter
// in the configuration until it has been unhealthy for the failover time
// (FilterFailedServerRemovals); the other removals are unchanged (MinQuorum).
type rolePromoter struct {
	// now is time.Now, replaced in tests.
	now    func() time.Time
	logger hclog.Logger

	mu sync.Mutex

	// lastStandbyChange is when a standby promotion or demotion last showed
	// in the state (the cooldown; observe).
	lastStandbyChange time.Time

	// manual is set by a manual failover until its promotion shows in the
	// state or the request is withdrawn: the standby promotion does not
	// wait for the cooldown nor for the failover time; preferred names the
	// standby it should choose.
	manual    bool
	preferred raft.ServerID

	// reasons keeps why the promoter asked for the change of a standby, for
	// the event written when the change shows in the state (observe).
	reasons map[raft.ServerID]standbyReason

	// voting is the suffrage of the standbys in the last observed state;
	// nil before the first observation.
	voting map[raft.ServerID]bool

	// lastWait is the last reason a due standby promotion did not happen,
	// and lastHold the last reason a voter was not demoted for its role;
	// each logged once per change.
	lastWait string
	lastHold string

	// promotions and demotions count the standby changes seen by this
	// server while it was the leader.
	promotions uint64
	demotions  uint64

	// onChange, if set, is called for every standby change observed in the
	// state (the Server event and the telemetry counter).
	onChange func(*structs.ServerRoleEvent)
}

// standbyReason is why the promoter changed the suffrage of a standby.
type standbyReason struct {
	reason   string
	replaced raft.ServerID
}

var _ autopilot.Promoter = (*rolePromoter)(nil)

func newRolePromoter(logger hclog.Logger) *rolePromoter {
	if logger == nil {
		logger = hclog.NewNullLogger()
	}
	return &rolePromoter{now: time.Now, logger: logger.Named("autopilot_promoter")}
}

// standbyConfig is the standby part of the autopilot configuration, passed
// to the promoter in autopilot.Config.Ext.
type standbyConfig struct {
	targetVoters  int
	failoverAfter time.Duration
	cooldown      time.Duration
}

func newStandbyConfig(c *structs.AutopilotConfig) *standbyConfig {
	if c == nil {
		return nil
	}
	return &standbyConfig{
		targetVoters:  int(c.TargetVoters),
		failoverAfter: c.StandbyFailoverAfterOrDefault(),
		cooldown:      c.StandbyCooldownOrDefault(),
	}
}

func standbyConfigOf(c *autopilot.Config) *standbyConfig {
	if sc, ok := c.Ext.(*standbyConfig); ok && sc != nil {
		return sc
	}
	return &standbyConfig{failoverAfter: structs.DefaultStandbyFailoverAfter, cooldown: structs.DefaultStandbyCooldown}
}

// GetServerExt keeps no extended server state.
func (*rolePromoter) GetServerExt(*autopilot.Config, *autopilot.ServerState) interface{} {
	return nil
}

// GetStateExt keeps no extended state.
func (*rolePromoter) GetStateExt(*autopilot.Config, *autopilot.State) interface{} {
	return nil
}

// GetNodeTypes reports the configured role of every server as its node type.
func (*rolePromoter) GetNodeTypes(_ *autopilot.Config, s *autopilot.State) map[raft.ServerID]autopilot.NodeType {
	types := make(map[raft.ServerID]autopilot.NodeType, len(s.Servers))
	for id, srv := range s.Servers {
		types[id] = autopilot.NodeType(serverRoleFromTag(srv.Server.Meta))
	}
	return types
}

// FilterFailedServerRemovals holds the removal of a failed voter back until
// the voter has been unhealthy for the failover time, where standbys replace
// voters (a target). Without it the dead server cleanup takes a voter out of
// the configuration as soon as Serf reports it failed, the voters fall below
// the target, and a standby is promoted at once: standby_failover_after
// would not hold for the commonest failure, and a short outage would change
// the voter set twice. Non-voters, and everything without a target, are
// left to autopilot's own rules (MinQuorum).
func (p *rolePromoter) FilterFailedServerRemovals(c *autopilot.Config, s *autopilot.State, failed *autopilot.FailedServers) *autopilot.FailedServers {
	sc := standbyConfigOf(c)
	if failed == nil || s == nil || sc.targetVoters == 0 {
		return failed
	}
	now := p.now()
	// due: unknown to the state or healthy there (autopilot decides), or
	// unhealthy for the failover time.
	due := func(id raft.ServerID) bool {
		srv, ok := s.Servers[id]
		if !ok || srv.Health.Healthy {
			return true
		}
		return now.Sub(srv.Health.StableSince) >= sc.failoverAfter
	}

	out := *failed
	out.StaleVoters = nil
	for _, id := range failed.StaleVoters {
		if due(id) {
			out.StaleVoters = append(out.StaleVoters, id)
		}
	}
	out.FailedVoters = nil
	for _, srv := range failed.FailedVoters {
		if srv == nil || due(srv.ID) {
			out.FailedVoters = append(out.FailedVoters, srv)
		}
	}
	return &out
}

// promoterView is the classification of the servers of one autopilot round.
type promoterView struct {
	now       time.Time
	minStable time.Duration

	// leaderIndex is the last index of the leader.
	leaderIndex uint64

	voters        int
	healthyVoters int

	// voterRoleVoters are the voters with the voter role (sorted IDs).
	voterRoleVoters []raft.ServerID

	// pendingVoters counts the healthy voter-role non-voters that are not
	// stable yet: they will be promoted soon.
	pendingVoters int

	// promotedStandbys are the voters with the standby role (with a target).
	promotedStandbys []raft.ServerID

	// standbys are the non-voters with the standby role.
	standbys []raft.ServerID

	// demote are the voters whose role does not vote.
	demote []raft.ServerID
}

// CalculatePromotionsAndDemotions applies the role rules described on
// rolePromoter.
func (p *rolePromoter) CalculatePromotionsAndDemotions(c *autopilot.Config, s *autopilot.State) autopilot.RaftChanges {
	p.mu.Lock()
	defer p.mu.Unlock()

	var changes autopilot.RaftChanges
	sc := standbyConfigOf(c)

	v := promoterView{now: p.now(), minStable: s.ServerStabilizationTime(c)}
	if leader, ok := s.Servers[s.Leader]; ok {
		v.leaderIndex = leader.Stats.LastIndex
	}

	// Sorted IDs make every decision deterministic.
	ids := make([]raft.ServerID, 0, len(s.Servers))
	for id := range s.Servers {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		srv := s.Servers[id]
		role := serverRoleFromTag(srv.Server.Meta)
		switch {
		case srv.State == autopilot.RaftNonVoter:
			switch {
			case role.Votes() && srv.Health.IsStable(v.now, v.minStable):
				changes.Promotions = append(changes.Promotions, id)
			case role.Votes() && srv.Health.Healthy:
				v.pendingVoters++
			case role == ServerRoleStandby:
				v.standbys = append(v.standbys, id)
			}
		case srv.HasVotingRights():
			v.voters++
			if srv.Health.Healthy {
				v.healthyVoters++
			}
			switch {
			case role.Votes():
				v.voterRoleVoters = append(v.voterRoleVoters, id)
			case role == ServerRoleStandby && sc.targetVoters > 0:
				v.promotedStandbys = append(v.promotedStandbys, id)
			default:
				v.demote = append(v.demote, id)
			}
		}
	}

	// Promotions of voter-role servers go first; autopilot applies
	// demotions only in a round without promotions.
	if len(changes.Promotions) > 0 {
		return changes
	}

	if sc.targetVoters > 0 {
		if id, ok := p.standbyPromotion(sc, s, &v); ok {
			changes.Promotions = []raft.ServerID{id}
			return changes
		}
	}

	if len(v.demote) > 0 {
		// Keep at least one voter with the voter role: never demote the
		// last voters of a cluster whose voter-role servers are all gone.
		if len(v.voterRoleVoters) == 0 {
			return changes
		}
		// And keep a quorum: the voters that are left must have a healthy
		// majority of their own. A voter restarted as an observer while
		// another voter is down would otherwise leave one healthy voter of
		// two, and the cluster without a leader.
		for _, id := range v.demote {
			if v.quorumWithout(s, id) {
				p.lastHold = ""
				return demoteOrHandOver(s, &v, id, changes)
			}
		}
		p.hold(fmt.Sprintf("%d of %d voters are healthy: without %s the others would have no healthy majority",
			v.healthyVoters, v.voters, v.demote[0]))
		return changes
	}
	p.lastHold = ""

	if sc.targetVoters > 0 {
		if id, ok := p.standbyDemotion(sc, s, &v); ok {
			return demoteOrHandOver(s, &v, id, changes)
		}
	}
	return changes
}

// quorumWithout reports whether the voters keep a healthy majority among
// themselves when the voter id loses its vote.
func (v *promoterView) quorumWithout(s *autopilot.State, id raft.ServerID) bool {
	others := v.voters - 1
	healthy := v.healthyVoters
	if srv, ok := s.Servers[id]; ok && srv.Health.Healthy {
		healthy--
	}
	return others > 0 && healthy >= others/2+1
}

// demoteOrHandOver demotes the voter, or, when it is the leader, moves the
// leadership to a healthy voter-role voter first; the demotion follows in a
// later round.
func demoteOrHandOver(s *autopilot.State, v *promoterView, id raft.ServerID, changes autopilot.RaftChanges) autopilot.RaftChanges {
	if id == s.Leader {
		for _, other := range v.voterRoleVoters {
			if s.Servers[other].Health.Healthy {
				changes.Leader = other
				return changes
			}
		}
		return changes
	}
	changes.Demotions = []raft.ServerID{id}
	return changes
}

// standbyPromotion returns the standby to promote, if one is due: the
// healthy voters are fewer than the target because a voter has been
// unhealthy for the failover time or a voter is missing from the
// configuration, no voter-role server is about to be promoted, the cooldown
// has passed, and a healthy, stable and caught-up standby exists. A manual
// failover waits neither for the failover time nor for the cooldown: a
// person has decided that the voter is lost.
//
// Nothing is noted as done here: the change counts when it shows in the
// state (observe). Until then every round asks for it again, which is the
// retry of a promotion that Raft did not apply.
func (p *rolePromoter) standbyPromotion(sc *standbyConfig, s *autopilot.State, v *promoterView) (raft.ServerID, bool) {
	if v.healthyVoters >= sc.targetVoters {
		p.lastWait = ""
		return "", false
	}

	failoverAfter := sc.failoverAfter
	if p.manual {
		failoverAfter = 0
	}

	// The voter to replace: the one unhealthy for the longest time beyond
	// the failover time; none when a voter is missing.
	var failed *autopilot.ServerState
	for _, srv := range s.Servers {
		if !srv.HasVotingRights() || srv.Health.Healthy {
			continue
		}
		if v.now.Sub(srv.Health.StableSince) < failoverAfter {
			continue
		}
		if failed == nil || srv.Health.StableSince.Before(failed.Health.StableSince) ||
			(srv.Health.StableSince.Equal(failed.Health.StableSince) && srv.Server.ID < failed.Server.ID) {
			failed = srv
		}
	}
	var reason string
	switch {
	case failed != nil:
		reason = fmt.Sprintf("voter %s unhealthy since %s", failed.Server.ID,
			failed.Health.StableSince.UTC().Format(time.RFC3339))
	case v.voters < sc.targetVoters:
		reason = fmt.Sprintf("%d voters, target %d", v.voters, sc.targetVoters)
	default:
		// A voter is unhealthy, but not for long enough yet.
		return "", false
	}

	if v.pendingVoters > 0 {
		p.wait("a voter-role server is about to be promoted")
		return "", false
	}
	if !p.manual && !p.lastStandbyChange.IsZero() && v.now.Sub(p.lastStandbyChange) < sc.cooldown {
		p.wait(fmt.Sprintf("standby cooldown until %s",
			p.lastStandbyChange.Add(sc.cooldown).UTC().Format(time.RFC3339)))
		return "", false
	}

	failedDC := ""
	var replaced raft.ServerID
	if failed != nil {
		failedDC = failed.Server.Meta["dc"]
		replaced = failed.Server.ID
	}
	id, lag, ok := p.standbyCandidate(s, v, failedDC)
	if !ok {
		p.wait("no healthy, stable and caught-up standby")
		return "", false
	}

	p.lastWait = ""
	if _, asked := p.reasons[id]; !asked {
		p.logger.Info("promoting standby", "standby", id, "replaces", replaced, "lag", lag, "reason", reason,
			"manual", p.manual)
	}
	p.setReason(id, standbyReason{reason: reason, replaced: replaced})
	return id, true
}

// standbyCandidate chooses the standby to promote among the healthy and
// stable ones (autopilot's health includes MaxTrailingLogs): the preferred
// one of a manual failover when it qualifies; otherwise from the datacenter
// of the failed voter first, then the most caught-up, then the lowest ID.
func (p *rolePromoter) standbyCandidate(s *autopilot.State, v *promoterView, dc string) (raft.ServerID, uint64, bool) {
	better := func(a, b *autopilot.ServerState) bool {
		if dc != "" {
			aDC, bDC := a.Server.Meta["dc"] == dc, b.Server.Meta["dc"] == dc
			if aDC != bDC {
				return aDC
			}
		}
		if a.Stats.LastIndex != b.Stats.LastIndex {
			return a.Stats.LastIndex > b.Stats.LastIndex
		}
		return a.Server.ID < b.Server.ID
	}
	var best *autopilot.ServerState
	for _, id := range v.standbys {
		srv := s.Servers[id]
		if !srv.Health.IsStable(v.now, v.minStable) {
			continue
		}
		if p.preferred != "" && id == p.preferred {
			best = srv
			break
		}
		if best == nil || better(srv, best) {
			best = srv
		}
	}
	if best == nil {
		return "", 0, false
	}
	var lag uint64
	if v.leaderIndex > best.Stats.LastIndex {
		lag = v.leaderIndex - best.Stats.LastIndex
	}
	return best.Server.ID, lag, true
}

// standbyDemotion returns the promoted standby that steps back, if one is
// due: the voters exceed the target, the other voters keep at least the
// target healthy, and the cooldown has passed. The least caught-up promoted
// standby steps back (then the highest ID).
func (p *rolePromoter) standbyDemotion(sc *standbyConfig, s *autopilot.State, v *promoterView) (raft.ServerID, bool) {
	if len(v.promotedStandbys) == 0 || v.voters <= sc.targetVoters {
		return "", false
	}
	var pick *autopilot.ServerState
	for _, id := range v.promotedStandbys {
		srv := s.Servers[id]
		if pick == nil || srv.Stats.LastIndex < pick.Stats.LastIndex ||
			(srv.Stats.LastIndex == pick.Stats.LastIndex && srv.Server.ID > pick.Server.ID) {
			pick = srv
		}
	}
	healthyOthers := v.healthyVoters
	if pick.Health.Healthy {
		healthyOthers--
	}
	if healthyOthers < sc.targetVoters {
		return "", false
	}
	if !p.lastStandbyChange.IsZero() && v.now.Sub(p.lastStandbyChange) < sc.cooldown {
		return "", false
	}
	if pick.Server.ID != s.Leader {
		// A leader hands over first; the cooldown starts when the demotion
		// shows in the state.
		reason := fmt.Sprintf("%d voters, target %d", v.voters, sc.targetVoters)
		if _, asked := p.reasons[pick.Server.ID]; !asked {
			p.logger.Info("demoting standby", "standby", pick.Server.ID, "reason", reason)
		}
		p.setReason(pick.Server.ID, standbyReason{reason: reason})
	}
	return pick.Server.ID, true
}

// wait logs once why a due standby promotion does not happen.
func (p *rolePromoter) wait(why string) {
	if p.lastWait == why {
		return
	}
	p.lastWait = why
	p.logger.Warn("standby promotion waits", "reason", why)
}

// hold logs once why a voter is not demoted for its role.
func (p *rolePromoter) hold(why string) {
	if p.lastHold == why {
		return
	}
	p.lastHold = why
	p.logger.Warn("role demotion waits", "reason", why)
}

func (p *rolePromoter) setReason(id raft.ServerID, r standbyReason) {
	if p.reasons == nil {
		p.reasons = make(map[raft.ServerID]standbyReason)
	}
	p.reasons[id] = r
}

// requestManualPromotion makes the standby promotion skip the cooldown and
// the failover time and choose the standby id when it qualifies (operator
// raft promote-standby), until the promotion shows in the state or the
// request is withdrawn.
func (p *rolePromoter) requestManualPromotion(id raft.ServerID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.manual = true
	p.preferred = id
}

// clearManualPromotion withdraws a manual request that did not lead to a
// promotion.
func (p *rolePromoter) clearManualPromotion() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.manual = false
	p.preferred = ""
}

// standbyCounts returns the standby promotions and demotions seen by this
// server as the leader.
func (p *rolePromoter) standbyCounts() (promotions, demotions uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.promotions, p.demotions
}

// observe compares the suffrage of the standbys with the last observed
// state and reports every change through onChange. The first state seen in
// a leadership is the baseline and reports nothing. A change that shows
// here starts the cooldown, and a promotion ends a manual request.
func (p *rolePromoter) observe(s *autopilot.State) {
	p.mu.Lock()
	var events []*structs.ServerRoleEvent
	voting := make(map[raft.ServerID]bool)
	for id, srv := range s.Servers {
		if serverRoleFromTag(srv.Server.Meta) != ServerRoleStandby {
			continue
		}
		votes := srv.HasVotingRights()
		voting[id] = votes
		if p.voting == nil {
			continue
		}
		before, known := p.voting[id]
		if !known || before == votes {
			continue
		}
		r := p.reasons[id]
		delete(p.reasons, id)
		ev := &structs.ServerRoleEvent{
			ID:         string(id),
			Name:       srv.Server.Name,
			Address:    string(srv.Server.Address),
			Datacenter: srv.Server.Meta["dc"],
			Role:       string(ServerRoleStandby),
			Voter:      votes,
			Reason:     r.reason,
			Replaced:   string(r.replaced),
		}
		p.lastStandbyChange = p.now()
		if votes {
			p.promotions++
			ev.Type = structs.TypeStandbyPromoted
			p.manual = false
			p.preferred = ""
		} else {
			p.demotions++
			ev.Type = structs.TypeStandbyDemoted
		}
		events = append(events, ev)
	}
	p.voting = voting
	onChange := p.onChange
	p.mu.Unlock()

	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	if onChange != nil {
		for _, ev := range events {
			onChange(ev)
		}
	}
}

// reset forgets the observed state, so a new leadership starts with a new
// baseline.
func (p *rolePromoter) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.voting = nil
	p.reasons = nil
	p.lastWait = ""
	p.lastHold = ""
	p.manual = false
	p.preferred = ""
}

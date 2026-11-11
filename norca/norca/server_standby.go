// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"net/http"
	"time"

	metrics "github.com/hashicorp/go-metrics"
	"github.com/hashicorp/raft"
	autopilot "github.com/hashicorp/raft-autopilot"

	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// promoteStandbyTimeout is how long a manual failover waits for autopilot
// to promote the standby.
var promoteStandbyTimeout = 30 * time.Second

// RaftPromoteStandby is the manual failover to a standby server (clean room,
// design raft-observers-and-hot-standby.md, section 6): after its checks it
// removes the failed voter named by Replace from the Raft configuration and
// lets autopilot promote the standby, without the cooldown. Autopilot stays
// the only owner of the voter set; the call waits for the promotion.
func (op *Operator) RaftPromoteStandby(args *structs.RaftPromoteStandbyRequest, reply *structs.RaftPromoteStandbyResponse) error {
	authErr := op.srv.Authenticate(op.ctx, args)
	if done, err := op.srv.forward("Operator.RaftPromoteStandby", args, args, reply); done {
		return err
	}
	op.srv.MeasureRPCRate("operator", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	if aclObj, err := op.srv.ResolveACL(args); err != nil {
		return err
	} else if aclObj != nil && !aclObj.AllowOperatorWrite() {
		return structs.ErrPermissionDenied
	}

	conf, err := op.srv.promoteStandby(args)
	if err != nil {
		return err
	}
	reply.Configuration = *conf
	reply.Index = conf.Index
	return nil
}

// promoteStandby runs the checks and the failover of RaftPromoteStandby on
// the leader.
func (s *Server) promoteStandby(args *structs.RaftPromoteStandbyRequest) (*structs.RaftConfigurationResponse, error) {
	badRequest := func(format string, a ...interface{}) error {
		return structs.NewErrRPCCoded(http.StatusBadRequest, fmt.Sprintf(format, a...))
	}
	if args.ID == "" {
		return nil, badRequest("the ID of the standby server is required")
	}
	if s.rolePromoter == nil {
		return nil, fmt.Errorf("the role promoter of autopilot is not running")
	}

	apConf := s.getOrCreateAutopilotConfig()
	if apConf == nil {
		return nil, fmt.Errorf("the autopilot configuration is not available yet")
	}
	if apConf.TargetVoters == 0 {
		return nil, badRequest("autopilot target_voters is 0, so nothing promotes a standby: " +
			"set it first (norca operator autopilot set-config -target-voters)")
	}

	state := s.autopilot.GetState()
	if state == nil {
		return nil, fmt.Errorf("the autopilot state is not available yet")
	}
	now := time.Now()

	standby, ok := state.Servers[args.ID]
	switch {
	case !ok:
		return nil, structs.NewErrRPCCoded(http.StatusNotFound,
			fmt.Sprintf("server %q is not in the Raft configuration seen by autopilot", args.ID))
	case serverRoleFromTag(standby.Server.Meta) != ServerRoleStandby:
		return nil, badRequest("server %q has the role %q, not %q", args.ID,
			serverRoleFromTag(standby.Server.Meta), ServerRoleStandby)
	case standby.HasVotingRights():
		return nil, badRequest("server %q is already a voter", args.ID)
	case !standby.Health.Healthy:
		return nil, badRequest("standby %q is not healthy (last contact %s, last index %d): it is not promoted",
			args.ID, standby.Stats.LastContact, standby.Stats.LastIndex)
	}
	stabilization := state.ServerStabilizationTime((&AutopilotDelegate{s}).AutopilotConfig())
	if !standby.Health.IsStable(now, stabilization) {
		return nil, badRequest("standby %q is healthy for less than %s: try again shortly", args.ID, stabilization)
	}

	if args.Replace != "" {
		failed, ok := state.Servers[args.Replace]
		switch {
		case !ok:
			return nil, structs.NewErrRPCCoded(http.StatusNotFound,
				fmt.Sprintf("server %q is not in the Raft configuration seen by autopilot", args.Replace))
		case !failed.HasVotingRights():
			return nil, badRequest("server %q is not a voter", args.Replace)
		case args.Replace == state.Leader:
			return nil, badRequest("server %q is the leader: it is not replaced", args.Replace)
		case failed.Health.Healthy && failed.Server.NodeStatus == autopilot.NodeAlive && !args.Force:
			return nil, badRequest("voter %q is healthy: use -force to replace it anyway", args.Replace)
		}
	} else {
		healthy := 0
		for _, srv := range state.Servers {
			if srv.HasVotingRights() && srv.Health.Healthy {
				healthy++
			}
		}
		if healthy >= int(apConf.TargetVoters) {
			return nil, badRequest("%d healthy voters meet target_voters %d: name the failed voter with -replace",
				healthy, apConf.TargetVoters)
		}
	}

	s.rolePromoter.requestManualPromotion(args.ID)

	if args.Replace != "" {
		// By ID: the address is the one of the Raft configuration.
		if err := s.raft.RemoveServer(args.Replace, 0, 0).Error(); err != nil {
			s.rolePromoter.clearManualPromotion()
			return nil, fmt.Errorf("removing voter %q failed, the standby is not promoted: %w", args.Replace, err)
		}
		s.logger.Warn("manual failover: removed the voter from the Raft configuration",
			"voter", args.Replace, "standby", args.ID)
	}

	deadline := time.NewTimer(promoteStandbyTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		future := s.raft.GetConfiguration()
		if err := future.Error(); err != nil {
			s.rolePromoter.clearManualPromotion()
			return nil, err
		}
		for _, srv := range future.Configuration().Servers {
			if srv.ID == args.ID && srv.Suffrage == raft.Voter {
				s.logger.Info("manual failover: standby promoted", "standby", args.ID, "replaced", args.Replace)
				return s.raftConfiguration()
			}
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			s.rolePromoter.clearManualPromotion()
			return nil, fmt.Errorf("standby %q was not promoted within %s: see norca operator autopilot health and the log of the leader",
				args.ID, promoteStandbyTimeout)
		case <-s.shutdownCh:
			return nil, fmt.Errorf("server shutting down")
		}
	}
}

// standbyChanged is called for every standby promotion or demotion seen by
// the leader's autopilot: one log line, a telemetry counter and a Server
// event.
func (s *Server) standbyChanged(ev *structs.ServerRoleEvent) {
	switch ev.Type {
	case structs.TypeStandbyPromoted:
		s.logger.Info("standby server promoted", "standby", ev.ID, "name", ev.Name,
			"replaces", ev.Replaced, "reason", ev.Reason)
		metrics.IncrCounter([]string{"nomad", "autopilot", "standby_promotions"}, 1)
	case structs.TypeStandbyDemoted:
		s.logger.Info("standby server demoted", "standby", ev.ID, "name", ev.Name, "reason", ev.Reason)
		metrics.IncrCounter([]string{"nomad", "autopilot", "standby_demotions"}, 1)
	}
	s.publishServerEvent(ev)
}

// publishServerEvent publishes an event of the Server topic. The event is
// not written to Raft, so only the leader that saw the change publishes it.
func (s *Server) publishServerEvent(ev *structs.ServerRoleEvent) {
	store := s.State()
	if store == nil {
		return
	}
	broker, err := store.EventBroker()
	if err != nil {
		return
	}
	index, _ := store.LatestIndex()
	broker.Publish(&structs.Events{
		Index: index,
		Events: []structs.Event{{
			Topic:      structs.TopicServer,
			Type:       ev.Type,
			Key:        ev.ID,
			FilterKeys: []string{ev.Name},
			Index:      index,
			Payload:    ev,
		}},
	})
}

// standbyGauges returns the voters, the standbys ready for a promotion
// (healthy non-voters with the standby role) and the standby promotions and
// demotions of this leader, from the autopilot state.
func (s *Server) standbyGauges() (voters, standbysReady int, promotions, demotions uint64) {
	if s.autopilot == nil {
		return
	}
	if state := s.autopilot.GetState(); state != nil {
		for _, srv := range state.Servers {
			switch {
			case srv.HasVotingRights():
				voters++
			case serverRoleFromTag(srv.Server.Meta) == ServerRoleStandby && srv.Health.Healthy:
				standbysReady++
			}
		}
	}
	if s.rolePromoter != nil {
		promotions, demotions = s.rolePromoter.standbyCounts()
	}
	return
}

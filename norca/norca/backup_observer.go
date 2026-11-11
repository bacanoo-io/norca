// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/raft"
	autopilot "github.com/hashicorp/raft-autopilot"

	"github.com/bacanoo-io/norca/norca/norca/backup"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// Backups taken by observers (clean room, design raft-observers-and-hot-
// standby.md, phase 3): with backup prefer_observer the leader asks a healthy
// observer to take the scheduled and manual backups from its own state; the
// observer uploads them and returns the record, which the leader writes to
// Raft. The leader takes a backup itself when no observer can.

// observerCandidates returns the observers that may take a backup, the most
// caught-up first: observers that autopilot reports healthy and alive.
func (s *Server) observerCandidates() []*autopilot.ServerState {
	if s.autopilot == nil {
		return nil
	}
	state := s.autopilot.GetState()
	if state == nil {
		return nil
	}
	var out []*autopilot.ServerState
	for _, srv := range state.Servers {
		if serverRoleFromTag(srv.Server.Meta) != ServerRoleObserver || srv.HasVotingRights() {
			continue
		}
		if !srv.Health.Healthy || srv.Server.NodeStatus != autopilot.NodeAlive {
			continue
		}
		out = append(out, srv)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Stats.LastIndex != out[j].Stats.LastIndex {
			return out[i].Stats.LastIndex > out[j].Stats.LastIndex
		}
		return out[i].Server.ID < out[j].Server.ID
	})
	return out
}

// delegateBackup is backup.Deps.Delegate on the leader: it asks the
// observers, the most caught-up first, until one takes the backup.
func (s *Server) delegateBackup(ctx context.Context, req *backup.DelegateRequest) (*structs.BackupRecord, error) {
	candidates := s.observerCandidates()
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%w: no healthy observer", backup.ErrDelegateRefused)
	}
	_, leaderID := s.raft.LeaderWithID()
	args := &structs.BackupTakeForLeaderRequest{
		LeaderID:     string(leaderID),
		ID:           req.ID,
		Trigger:      req.Trigger,
		Labels:       req.Labels,
		Pin:          req.Pin,
		Fingerprint:  req.Fingerprint,
		WriteRequest: structs.WriteRequest{Region: s.config.Region},
	}

	var refusals []string
	for _, c := range candidates {
		s.peerLock.RLock()
		parts := s.localPeers[c.Server.Address]
		s.peerLock.RUnlock()
		if parts == nil {
			refusals = append(refusals, c.Server.Name+": unknown address")
			continue
		}

		var reply structs.BackupTakeForLeaderResponse
		done := make(chan error, 1)
		go func() {
			done <- s.connPool.RPC(s.config.Region, parts.Addr, "Backup.TakeForLeader", args, &reply)
		}()
		select {
		case err := <-done:
			if err != nil {
				// The observer may have started: the outcome is unknown.
				return nil, fmt.Errorf("observer %s: %w", c.Server.Name, err)
			}
		case <-ctx.Done():
			return nil, fmt.Errorf("observer %s did not answer: %w", c.Server.Name, ctx.Err())
		}
		if reply.Refused != "" {
			s.logger.Debug("observer refused a backup", "observer", c.Server.Name, "id", req.ID, "reason", reply.Refused)
			refusals = append(refusals, c.Server.Name+": "+reply.Refused)
			continue
		}
		if reply.Backup == nil {
			return nil, fmt.Errorf("observer %s returned no record", c.Server.Name)
		}
		return reply.Backup, nil
	}
	return nil, fmt.Errorf("%w: %s", backup.ErrDelegateRefused, strings.Join(refusals, "; "))
}

// TakeForLeader takes a backup for the leader on this observer and returns
// its record (backup prefer_observer). Only another server of the region may
// call it (a server certificate where TLS verifies server names), only for
// the current leader, and only on an observer. A refusal is an answer, not
// an error: it tells the leader that nothing was started.
func (b *Backup) TakeForLeader(args *structs.BackupTakeForLeaderRequest, reply *structs.BackupTakeForLeaderResponse) error {
	refuse := func(format string, a ...any) error {
		reply.Refused = fmt.Sprintf(format, a...)
		return nil
	}
	if err := validateTLSCertificateLevel(b.srv, b.ctx, tlsCertificateLevelServer); err != nil {
		return refuse("not called by a server: %v", err)
	}
	if args.Region != b.srv.config.Region {
		return refuse("this server is in region %q, not %q", b.srv.config.Region, args.Region)
	}
	if role, _ := ParseServerRole(string(b.srv.config.Role)); role != ServerRoleObserver {
		return refuse("this server has the role %q, not %q", role, ServerRoleObserver)
	}
	if _, leader := b.srv.raft.LeaderWithID(); leader == "" || leader != raft.ServerID(args.LeaderID) {
		return refuse("%q is not the current leader", args.LeaderID)
	}
	if b.srv.backup == nil {
		return refuse("backups are not set up on this server")
	}

	record, err := b.srv.backup.TakeDelegated(b.srv.shutdownCtx, &backup.DelegateRequest{
		ID:          args.ID,
		Trigger:     args.Trigger,
		Labels:      args.Labels,
		Pin:         args.Pin,
		Fingerprint: args.Fingerprint,
	})
	if errors.Is(err, backup.ErrDelegateRefused) {
		return refuse("%v", err)
	}
	if err != nil {
		return err
	}
	reply.Backup = record
	return nil
}

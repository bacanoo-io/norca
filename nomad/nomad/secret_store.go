// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"strconv"

	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/secretstore"
)

// setupSecretStore creates the storage and HA lock backend of the built-in
// secret store on this server's Raft (see nomad/secretstore). Nothing uses it
// yet: the seal and the store of later phases create the uBixVault core over
// it. Leadership changes reach it from establishLeadership and
// revokeLeadership.
func (s *Server) setupSecretStore() {
	s.secretStore = secretstore.New(&secretstore.Deps{
		Logger: s.logger.Named("secret_store"),
		State:  func() *state.StateStore { return s.fsm.State() },
		Apply: func(msgType structs.MessageType, req any) (uint64, error) {
			_, index, err := s.raftApply(msgType, req)
			return index, err
		},
		ServersReady: func() bool {
			return ServersMeetMinimumNorcaVersion(s.Members(), s.Region(), false)
		},
		Leader: func() (string, string) {
			addr, id := s.raft.LeaderWithID()
			return string(id), string(addr)
		},
		Term: s.raftTerm,
	})
}

// raftTerm returns the current Raft term of this server.
func (s *Server) raftTerm() uint64 {
	term, _ := strconv.ParseUint(s.raft.Stats()["term"], 10, 64)
	return term
}

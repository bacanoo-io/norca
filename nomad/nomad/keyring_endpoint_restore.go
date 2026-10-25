// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"net/http"
	"slices"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/nomad/nomad/structs"
)

// maxRestoreKeys bounds the root keys of a single keyring restore.
const maxRestoreKeys = 1000

// Restore writes root keys of a backup into the keystore of every server of
// the region, alongside the existing keys; no key is removed or replaced.
// It is used before the Raft state of a backup is restored (full restore,
// or building a new cluster from a backup), so the Variables and workload
// identities of the backup stay readable. The key metadata is not written
// to Raft: it comes with the restored state.
//
// The server that receives the request adds the keys to its own keystore
// first, then sends them to the other servers with LocalOnly set, which
// only servers may do.
func (k *Keyring) Restore(args *structs.KeyringRestoreRequest, reply *structs.KeyringRestoreResponse) error {
	authErr := k.srv.Authenticate(k.ctx, args)
	if args.LocalOnly {
		// The fan-out of another server of the region: never forwarded.
		if err := validateTLSCertificateLevel(k.srv, k.ctx, tlsCertificateLevelServer); err != nil {
			return err
		}
	} else if done, err := k.srv.forward("Keyring.Restore", args, args, reply); done {
		return err
	}
	k.srv.MeasureRPCRate("keyring", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "keyring", "restore"}, time.Now())

	if aclObj, err := k.srv.ResolveACL(args); err != nil {
		return err
	} else if aclObj != nil && !aclObj.IsManagement() {
		return structs.ErrPermissionDenied
	}
	if len(args.Keys) == 0 {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "no root keys to restore")
	}
	if len(args.Keys) > maxRestoreKeys {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "at most %d root keys can be restored at once", maxRestoreKeys)
	}

	if args.LocalOnly {
		added, err := k.encrypter.restoreKeys(args.Keys)
		reply.Added = added
		if err != nil {
			return err
		}
		reply.Servers = []string{k.srv.config.NodeName}
		k.logger.Info("restored root keys from a backup", "added", added)
		return nil
	}

	resp, err := k.srv.restoreKeyring(args.Keys, args.AuthToken)
	if err != nil {
		return err
	}
	*reply = *resp
	reply.Index, _ = k.srv.State().LatestIndex()
	return nil
}

// restoreKeyring adds root keys to the keystore of this server, then of
// every other server of the region. Failing to add them locally is an
// error; the failures of other servers are returned in the response, as
// the keyring replication fetches missing keys from the leader too.
func (s *Server) restoreKeyring(keys []*structs.RootKey, authToken string) (*structs.KeyringRestoreResponse, error) {
	if s.encrypter == nil {
		return nil, fmt.Errorf("keyring is not initialized")
	}
	reply := &structs.KeyringRestoreResponse{Errors: map[string]string{}}
	added, err := s.encrypter.restoreKeys(keys)
	reply.Added = added
	if err != nil {
		return reply, err
	}
	reply.Servers = append(reply.Servers, s.config.NodeName)
	s.logger.Named("keyring").Info("restored root keys from a backup", "added", added)

	s.peerLock.RLock()
	peers := make([]*serverParts, 0, len(s.localPeers))
	for _, peer := range s.localPeers {
		if peer.ID != s.config.NodeID {
			peers = append(peers, peer.Copy())
		}
	}
	s.peerLock.RUnlock()

	for _, peer := range peers {
		req := &structs.KeyringRestoreRequest{
			Keys:      keys,
			LocalOnly: true,
			WriteRequest: structs.WriteRequest{
				Region:    s.config.Region,
				AuthToken: authToken,
			},
		}
		var resp structs.KeyringRestoreResponse
		if err := s.forwardServer(peer, "Keyring.Restore", req, &resp); err != nil {
			reply.Errors[peer.Name] = err.Error()
			s.logger.Named("keyring").Warn("failed to restore root keys on a server", "server", peer.Name, "error", err)
			continue
		}
		reply.Servers = append(reply.Servers, peer.Name)
	}
	slices.Sort(reply.Servers)
	return reply, nil
}

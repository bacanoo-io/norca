// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/nomad/nomad/seal"
	"github.com/hashicorp/nomad/nomad/structs"
)

// isLocalServer reports whether a server name (with or without the region
// suffix of its Serf name) or ID names this server.
func (s *Server) isLocalServer(id string) bool {
	if id == s.config.NodeName || id == s.config.NodeID || id == string(s.config.RaftConfig.LocalID) {
		return true
	}
	return s.serf != nil && id == s.LocalMember().Name
}

// findServer returns a server of the region by ID, Serf name, or node name.
func (s *Server) findServer(id string) (*serverParts, error) {
	target, err := s.getServer(s.Region(), id)
	if err != nil {
		if alt, err2 := s.getServer(s.Region(), id+"."+s.Region()); err2 == nil {
			return alt, nil
		}
	}
	return target, err
}

// SealStatus returns the seal status of one server (ServerID) or of every
// server of the region. It needs operator:read.
func (op *Operator) SealStatus(args *structs.SealStatusRequest, reply *structs.SealStatusResponse) error {
	authErr := op.srv.Authenticate(op.ctx, args)
	if region := args.RequestRegion(); region != op.srv.Region() {
		args.SetForwarded()
		return op.srv.forwardRegion(region, "Operator.SealStatus", args, reply)
	}
	op.srv.MeasureRPCRate("operator", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	if aclObj, err := op.srv.ResolveACL(args); err != nil {
		return err
	} else if aclObj != nil && !aclObj.AllowOperatorRead() {
		return structs.ErrPermissionDenied
	}

	if args.ServerID != "" && !op.srv.isLocalServer(args.ServerID) {
		target, err := op.srv.findServer(args.ServerID)
		if err != nil {
			return err
		}
		return op.srv.forwardServer(target, "Operator.SealStatus", args, reply)
	}

	if cfg, err := op.srv.State().SealConfig(nil); err != nil {
		return err
	} else {
		reply.Config = cfg.Stub()
	}
	reply.Servers = []*structs.ServerSealStatus{op.srv.seal.status()}
	if args.ServerID == "" {
		op.srv.peerLock.RLock()
		peers := make([]*serverParts, 0, len(op.srv.peers[op.srv.Region()]))
		for _, p := range op.srv.peers[op.srv.Region()] {
			if !op.srv.isLocalServer(p.ID) {
				peers = append(peers, p.Copy())
			}
		}
		op.srv.peerLock.RUnlock()

		for _, p := range peers {
			req := *args
			req.ServerID = p.ID
			req.AllowStale = true
			var resp structs.SealStatusResponse
			if err := op.srv.forwardServer(p, "Operator.SealStatus", &req, &resp); err != nil || len(resp.Servers) == 0 {
				msg := "no status"
				if err != nil {
					msg = err.Error()
				}
				reply.Servers = append(reply.Servers, &structs.ServerSealStatus{
					ID: p.ID, Name: p.Name, Address: p.Addr.String(), Error: msg,
				})
				continue
			}
			reply.Servers = append(reply.Servers, resp.Servers[0])
		}
	}
	sort.Slice(reply.Servers, func(i, j int) bool { return reply.Servers[i].Name < reply.Servers[j].Name })
	op.srv.setQueryMeta(&reply.QueryMeta)
	reply.Index, _ = op.srv.State().Index("seal_config")
	return nil
}

// SealUnseal enters one unseal share on a server, or discards the shares
// entered so far (Reset). It needs operator:write. The share is never
// logged.
func (op *Operator) SealUnseal(args *structs.SealUnsealRequest, reply *structs.SealUnsealResponse) error {
	authErr := op.srv.Authenticate(op.ctx, args)
	if region := args.RequestRegion(); region != op.srv.Region() {
		args.SetForwarded()
		return op.srv.forwardRegion(region, "Operator.SealUnseal", args, reply)
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
	defer metrics.MeasureSince([]string{"nomad", "seal", "unseal"}, time.Now())

	if args.ServerID != "" && !op.srv.isLocalServer(args.ServerID) {
		target, err := op.srv.findServer(args.ServerID)
		if err != nil {
			return err
		}
		return op.srv.forwardServer(target, "Operator.SealUnseal", args, reply)
	}

	m := op.srv.seal
	switch {
	case args.Reset:
		m.shares.ResetProgress()
	case args.Key == "":
		return structs.NewErrRPCCoded(http.StatusBadRequest, "an unseal share is required")
	default:
		share, err := decodeShare(args.Key)
		if err != nil {
			return structs.NewErrRPCCoded(http.StatusBadRequest, err.Error())
		}
		err = m.unseal(share)
		clear(share)
		switch {
		case errors.Is(err, seal.ErrInvalidShare):
			return structs.NewErrRPCCoded(http.StatusBadRequest, err.Error())
		case errors.Is(err, seal.ErrUnsealFailed):
			op.logger.Warn("unseal failed: the shares entered did not reconstruct the unseal key")
			return structs.NewErrRPCCoded(http.StatusBadRequest, err.Error())
		case err != nil:
			return err
		}
	}
	reply.Status = m.status()
	reply.Index, _ = op.srv.State().LatestIndex()
	return nil
}

// decodeShare decodes a base64 unseal share.
func decodeShare(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			if len(b) != seal.ShareSize {
				return nil, seal.ErrInvalidShare
			}
			return b, nil
		}
	}
	return nil, fmt.Errorf("%w: not base64", seal.ErrInvalidShare)
}

// SealRewrapKey returns the key of a seal of the rewrap in progress to
// another server of the region, which wraps its keys with it. Only servers
// may call it, as for the root keys of Keyring.Get.
func (op *Operator) SealRewrapKey(args *structs.SealRewrapKeyRequest, reply *structs.SealRewrapKeyResponse) error {
	authErr := op.srv.Authenticate(op.ctx, args)
	if err := validateTLSCertificateLevel(op.srv, op.ctx, tlsCertificateLevelServer); err != nil {
		return err
	}
	if done, err := op.srv.forward("Operator.SealRewrapKey", args, args, reply); done {
		return err
	}
	op.srv.MeasureRPCRate("operator", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	key, err := op.srv.seal.rewrapKey(args.RewrapID, args.KeyID)
	if err != nil {
		return err
	}
	reply.Key = key
	op.srv.setQueryMeta(&reply.QueryMeta)
	return nil
}

// SealRewrapDone records on the leader that a server has wrapped its keys
// with the seals of a rewrap. Only servers may call it.
func (op *Operator) SealRewrapDone(args *structs.SealRewrapDoneRequest, reply *structs.GenericResponse) error {
	authErr := op.srv.Authenticate(op.ctx, args)
	if err := validateTLSCertificateLevel(op.srv, op.ctx, tlsCertificateLevelServer); err != nil {
		return err
	}
	if done, err := op.srv.forward("Operator.SealRewrapDone", args, args, reply); done {
		return err
	}
	op.srv.MeasureRPCRate("operator", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	if args.RewrapID == "" || args.ServerID == "" {
		return structs.NewErrRPCCoded(http.StatusBadRequest, "rewrap and server IDs are required")
	}
	_, index, err := op.srv.raftApply(structs.SealRewrapDoneRequestType|structs.IgnoreUnknownTypeFlag, args)
	if err != nil {
		return err
	}
	reply.Index = index
	return nil
}

// Rewrap moves the keys of the region to another seal online (Norca): every
// server adds wraps of the new seal to its keys, and once all are done the
// old wraps are dropped; until then both seals unwrap the keys. A rewrap to
// shares generates the unseal key and returns its shares once; in a new
// region configured with a shares seal it initializes the keyring. It needs
// operator:write.
func (k *Keyring) Rewrap(args *structs.KeyringRewrapRequest, reply *structs.KeyringRewrapResponse) error {
	authErr := k.srv.Authenticate(k.ctx, args)
	if done, err := k.srv.forward("Keyring.Rewrap", args, args, reply); done {
		return err
	}
	k.srv.MeasureRPCRate("keyring", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	if aclObj, err := k.srv.ResolveACL(args); err != nil {
		return err
	} else if aclObj != nil && !aclObj.AllowOperatorWrite() {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "keyring", "rewrap"}, time.Now())

	resp, err := k.srv.seal.startRewrap(args)
	if err != nil {
		if structs.IsErrSealed(err) {
			return err
		}
		return structs.NewErrRPCCoded(http.StatusBadRequest, err.Error())
	}
	*reply = *resp
	k.logger.Info("keyring rewrap requested", "audit", true, "to", args.To, "rewrap_id", resp.RewrapID)
	return nil
}

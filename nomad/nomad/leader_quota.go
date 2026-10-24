// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"bytes"
	"context"

	"golang.org/x/time/rate"

	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
)

// replicateQuotaSpecs is used to replicate quota specifications from the
// authoritative region to this region. Quota usage is not replicated: every
// region tracks the usage of its own allocations against its own limit.
func (s *Server) replicateQuotaSpecs(stopCh chan struct{}) {
	req := structs.QuotaSpecListRequest{
		QueryOptions: structs.QueryOptions{
			Region:     s.config.AuthoritativeRegion,
			AllowStale: true,
		},
	}
	limiter := rate.NewLimiter(replicationRateLimit, int(replicationRateLimit))
	s.logger.Debug("starting quota replication from authoritative region", "region", req.Region)

	for {
		select {
		case <-stopCh:
			return
		default:
		}

		// Rate limit how often we attempt replication
		_ = limiter.Wait(context.Background())

		if !ServersMeetMinimumVersion(s.serf.Members(), s.Region(), minNorcaFeaturesVersion, true) {
			s.logger.Trace("all servers must be upgraded before quotas can be replicated",
				"min_version", minNorcaFeaturesVersion)
			if s.replicationBackoffContinue(stopCh) {
				continue
			}
			return
		}

		var resp structs.QuotaSpecListResponse
		req.AuthToken = s.ReplicationToken()
		err := s.forwardRegion(s.config.AuthoritativeRegion, "Quota.ListQuotaSpecs", &req, &resp)
		if err != nil {
			s.logger.Error("failed to fetch quotas from authoritative region", "error", err)
			if s.replicationBackoffContinue(stopCh) {
				continue
			}
			return
		}

		// Perform a two-way diff
		delete, update := diffQuotaSpecs(s.State(), req.MinQueryIndex, resp.Quotas)

		select {
		case <-stopCh:
			return
		default:
		}

		// Update first so namespaces can be switched to new quotas before old
		// ones are deleted.
		if len(update) > 0 {
			args := &structs.QuotaSpecUpsertRequest{Quotas: update}
			_, _, err := s.raftApply(structs.QuotaSpecUpsertRequestType|structs.IgnoreUnknownTypeFlag, args)
			if err != nil {
				s.logger.Error("failed to update quotas", "error", err)
				if s.replicationBackoffContinue(stopCh) {
					continue
				}
				return
			}
		}

		if len(delete) > 0 {
			args := &structs.QuotaSpecDeleteRequest{Names: delete}
			_, _, err := s.raftApply(structs.QuotaSpecDeleteRequestType|structs.IgnoreUnknownTypeFlag, args)
			if err != nil {
				// A quota may still be referenced by a namespace that has not
				// been replicated yet; retry later.
				s.logger.Error("failed to delete quotas", "error", err)
				if s.replicationBackoffContinue(stopCh) {
					continue
				}
				return
			}
		}

		// Update the minimum query index, blocks until there is a change.
		req.MinQueryIndex = resp.Index
	}
}

// diffQuotaSpecs is used to perform a two-way diff between the local quota
// specifications and the remote ones to determine which need to be deleted
// or updated.
func diffQuotaSpecs(store *state.StateStore, minIndex uint64, remoteList []*structs.QuotaSpec) (delete []string, update []*structs.QuotaSpec) {
	local := make(map[string][]byte)
	remote := make(map[string]struct{})

	iter, err := store.QuotaSpecs(nil, state.SortDefault)
	if err != nil {
		panic("failed to iterate local quotas")
	}
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		spec := raw.(*structs.QuotaSpec)
		local[spec.Name] = spec.Hash
	}

	for _, rspec := range remoteList {
		remote[rspec.Name] = struct{}{}

		if localHash, ok := local[rspec.Name]; !ok {
			update = append(update, rspec)
		} else if rspec.ModifyIndex > minIndex && !bytes.Equal(localHash, rspec.Hash) {
			update = append(update, rspec)
		}
	}

	for name := range local {
		if _, ok := remote[name]; !ok {
			delete = append(delete, name)
		}
	}
	return
}

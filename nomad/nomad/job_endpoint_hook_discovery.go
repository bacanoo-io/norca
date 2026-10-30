// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"math"

	"github.com/hashicorp/nomad/nomad/structs"
)

// setDiscoveryDefaultOverflowFraction sets the overflow fraction the server
// fills in at job registration for discovery blocks that leave it unset.
func (s *Server) setDiscoveryDefaultOverflowFraction(fraction float64) {
	s.discoveryOverflowFraction.Store(math.Float64bits(fraction))
}

// discoveryDefaultOverflowFraction returns the overflow fraction the server
// fills in at job registration for discovery blocks that leave it unset: the
// default_overflow_fraction of the server's native_service_discovery block,
// structs.DefaultDiscoveryOverflowFraction unless configured.
func (s *Server) discoveryDefaultOverflowFraction() float64 {
	return math.Float64frombits(s.discoveryOverflowFraction.Load())
}

// jobDiscoveryDefaultsHook resolves the unset overflow fraction of the
// discovery blocks of a job's services to the default of the servers of the
// region, so the stored job, the plan diff and the service registrations made
// from it always carry a concrete value. Explicit values, including 0, are
// kept. It runs on the leader of the region the job is registered in, so the
// unset form never reaches the Raft log; each region of a multiregion job
// applies its own default, as every region registers the job itself.
type jobDiscoveryDefaultsHook struct {
	srv *Server
}

func (jobDiscoveryDefaultsHook) Name() string {
	return "discovery-defaults"
}

func (h jobDiscoveryDefaultsHook) Mutate(job *structs.Job) (*structs.Job, []error, error) {
	setDiscoveryDefaults(job, h.srv.discoveryDefaultOverflowFraction())
	return job, nil, nil
}

// setDiscoveryDefaults fills in the overflow fraction of the discovery blocks
// of the services of job that leave it unset.
func setDiscoveryDefaults(job *structs.Job, overflowFraction float64) {
	if job == nil {
		return
	}
	for _, tg := range job.TaskGroups {
		if tg == nil {
			continue
		}
		for _, service := range tg.Services {
			if service != nil {
				service.Discovery.SetDefaultOverflowFraction(overflowFraction)
			}
		}
		for _, task := range tg.Tasks {
			if task == nil {
				continue
			}
			for _, service := range task.Services {
				if service != nil {
					service.Discovery.SetDefaultOverflowFraction(overflowFraction)
				}
			}
		}
	}
}

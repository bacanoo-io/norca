// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"errors"
	"net"
	"sort"
	"strconv"

	"github.com/hashicorp/nomad/api"
)

// Cluster is what the operator reads from the cluster besides its objects
// and jobs.
type Cluster interface {
	// ServiceTargets returns the healthy instances of a service: the
	// registrations of running allocations that are not marked unhealthy
	// by their deployment.
	ServiceTargets(ctx context.Context, namespace, service string) ([]Target, error)

	// JobEndpoints returns the "ip:port" addresses of the port with the
	// given label of the running allocations of a job.
	JobEndpoints(ctx context.Context, namespace, jobID, portLabel string) ([]string, error)
}

type nomadCluster struct {
	client *api.Client
}

func healthy(stub *api.AllocationListStub) bool {
	if stub.ClientStatus != api.AllocClientStatusRunning || stub.DesiredStatus != api.AllocDesiredStatusRun {
		return false
	}
	ds := stub.DeploymentStatus
	return ds == nil || ds.Healthy == nil || *ds.Healthy
}

func (c *nomadCluster) ServiceTargets(ctx context.Context, namespace, service string) ([]Target, error) {
	q := (&api.QueryOptions{Namespace: namespace}).WithContext(ctx)
	regs, _, err := c.client.Services().Get(service, q)
	if err != nil {
		var respErr api.UnexpectedResponseError
		if errors.As(err, &respErr) && respErr.StatusCode() == 404 {
			return nil, nil
		}
		return nil, err
	}

	allocs := map[string]*api.AllocationListStub{}
	seenJobs := map[string]bool{}
	for _, reg := range regs {
		if seenJobs[reg.JobID] {
			continue
		}
		seenJobs[reg.JobID] = true
		stubs, _, err := c.client.Jobs().Allocations(reg.JobID, false, q)
		if err != nil {
			return nil, err
		}
		for _, s := range stubs {
			allocs[s.ID] = s
		}
	}

	var out []Target
	for _, reg := range regs {
		if stub, ok := allocs[reg.AllocID]; !ok || !healthy(stub) {
			continue
		}
		out = append(out, Target{Address: reg.Address, Port: reg.Port, AllocID: reg.AllocID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

func (c *nomadCluster) JobEndpoints(ctx context.Context, namespace, jobID, portLabel string) ([]string, error) {
	q := (&api.QueryOptions{Namespace: namespace}).WithContext(ctx)
	stubs, _, err := c.client.Jobs().Allocations(jobID, false, q)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, stub := range stubs {
		if !healthy(stub) {
			continue
		}
		alloc, _, err := c.client.Allocations().Info(stub.ID, q)
		if err != nil {
			return nil, err
		}
		if alloc.AllocatedResources == nil {
			continue
		}
		for _, p := range alloc.AllocatedResources.Shared.Ports {
			if p.Label == portLabel {
				out = append(out, net.JoinHostPort(p.HostIP, strconv.Itoa(p.Value)))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

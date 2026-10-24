// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package allochealth

import (
	"context"
	"testing"
	"time"

	consulapi "github.com/hashicorp/consul/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/serviceregistration"
	"github.com/hashicorp/nomad/client/serviceregistration/checks/checkstore"
	regmock "github.com/hashicorp/nomad/client/serviceregistration/mock"
	"github.com/hashicorp/nomad/client/state"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/client/taskenv"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestTracker_NomadChecks_LivenessIgnored(t *testing.T) {
	ci.Parallel(t)

	alloc := mock.Alloc()
	alloc.Job.TaskGroups[0].Migrate.MinHealthyTime = 1
	task := alloc.Job.TaskGroups[0].Tasks[0]
	task.Services[0].Provider = structs.ServiceProviderNomad
	task.Services[0].Checks = []*structs.ServiceCheck{
		{Name: "ready", Role: structs.CheckRoleReadiness},
		{Name: "alive", Role: structs.CheckRoleLiveness},
	}

	logger := testlog.HCLogger(t)
	b := cstructs.NewAllocBroadcaster(logger)
	defer b.Close()

	alloc.ClientStatus = structs.AllocClientStatusRunning
	alloc.TaskStates = map[string]*structs.TaskState{
		task.Name: {State: structs.TaskStateRunning, StartedAt: time.Now()},
	}

	// the readiness check passes, the liveness check fails
	checks := checkstore.NewStore(logger, state.NewMemDB(logger))
	for _, r := range []*structs.CheckQueryResult{
		{ID: "ready", Mode: structs.Healthiness, Status: structs.CheckSuccess, Role: structs.CheckRoleReadiness},
		{ID: "alive", Mode: structs.Healthiness, Status: structs.CheckFailure, Role: structs.CheckRoleLiveness},
	} {
		must.NoError(t, checks.Set(alloc.ID, r))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	consul := regmock.NewServiceRegistrationHandler(logger)
	taskEnvBuilder := taskenv.NewBuilder(mock.Node(), alloc, nil, alloc.Job.Region)

	tracker := NewTracker(ctx, logger, alloc, b.Listen(), taskEnvBuilder, consul, checks, time.Millisecond, true)
	tracker.checkLookupInterval = 10 * time.Millisecond
	must.Eq(t, 1, tracker.nomadCheckCount)
	tracker.Start()

	select {
	case <-time.After(time.Second):
		t.Fatal("timed out while waiting for health")
	case h := <-tracker.HealthyCh():
		must.True(t, h)
	}
}

func TestTracker_countChecks_Liveness(t *testing.T) {
	ci.Parallel(t)

	services := []*structs.Service{
		{Provider: structs.ServiceProviderNomad, Checks: []*structs.ServiceCheck{
			{Name: "a"}, {Name: "b", Role: structs.CheckRoleLiveness}, {Name: "c", Role: structs.CheckRoleStartup},
		}},
		{Provider: structs.ServiceProviderConsul, Checks: []*structs.ServiceCheck{
			{Name: "d", Role: structs.CheckRoleLiveness},
		}},
	}

	consul, nomad := countChecks(services)
	must.Eq(t, 0, consul)
	must.Eq(t, 2, nomad)
}

func TestTracker_evaluateConsulChecks_Liveness(t *testing.T) {
	ci.Parallel(t)

	// the liveness check is not registered in consul; the others are
	services := []*structs.Service{{
		Name: "web",
		Checks: []*structs.ServiceCheck{
			{Name: "ready", Role: structs.CheckRoleReadiness},
			{Name: "alive", Role: structs.CheckRoleLiveness},
		},
	}}

	registrations := &serviceregistration.AllocRegistration{
		Tasks: map[string]*serviceregistration.ServiceRegistrations{
			"task": {
				Services: map[string]*serviceregistration.ServiceRegistration{
					"abc123": {
						ServiceID: "abc123",
						Checks: []*consulapi.AgentCheck{
							{Name: "ready", CheckID: "ready-id", Status: consulapi.HealthPassing},
						},
					},
				},
			},
		},
	}
	must.True(t, evaluateConsulChecks(services, registrations))

	registrations.Tasks["task"].Services["abc123"].Checks[0].Status = consulapi.HealthCritical
	must.False(t, evaluateConsulChecks(services, registrations))
}

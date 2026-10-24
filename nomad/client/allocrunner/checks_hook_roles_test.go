// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package allocrunner

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/allocrunner/interfaces"
	"github.com/hashicorp/nomad/client/taskenv"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

func allocWithRoleChecks(addr, port, provider string) *structs.Allocation {
	alloc := mock.Alloc()
	group := alloc.Job.LookupTaskGroup(alloc.TaskGroup)
	task := group.Tasks[0].Name

	check := func(name, path, role string) *structs.ServiceCheck {
		return &structs.ServiceCheck{
			Name:        name,
			Type:        "http",
			Path:        path,
			Protocol:    "http",
			PortLabel:   port,
			AddressMode: "auto",
			Interval:    100 * time.Millisecond,
			Timeout:     time.Second,
			Method:      "GET",
			TaskName:    task,
			Role:        role,
		}
	}

	group.Services = nil
	group.Tasks[0].Services = []*structs.Service{{
		Name:        "web",
		TaskName:    task,
		PortLabel:   port,
		AddressMode: "auto",
		Address:     addr,
		Provider:    provider,
		Checks: []*structs.ServiceCheck{
			check("legacy", "/fail", ""),
			check("alive", "/", structs.CheckRoleLiveness),
			check("ready", "/", structs.CheckRoleReadiness),
			check("started", "/started", structs.CheckRoleStartup),
		},
	}}
	return alloc
}

func statusesByName(results map[structs.CheckID]*structs.CheckQueryResult) map[string]structs.CheckStatus {
	m := make(map[string]structs.CheckStatus, len(results))
	for _, result := range results {
		m[result.Check] = result.Status
	}
	return m
}

func TestCheckHook_Roles_StartupGate(t *testing.T) {
	ci.Parallel(t)

	logger := testlog.HCLogger(t)

	var started atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/fail", r.URL.Path == "/started" && !started.Load():
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer ts.Close()

	tokens := strings.Split(ts.URL, ":")
	addr, port := strings.TrimPrefix(tokens[1], "//"), tokens[2]

	shim := makeCheckStore(logger)
	alloc := allocWithRoleChecks(addr, port, structs.ServiceProviderNomad)
	envBuilder := taskenv.NewBuilder(mock.Node(), alloc, nil, alloc.Job.Region)

	h := newChecksHook(logger, alloc, shim, mock.NewNetworkStatus(addr), envBuilder.Build())
	must.NoError(t, h.Prerun())
	defer h.PreKill()

	waitFor := func(exp map[string]structs.CheckStatus) {
		testutil.WaitForResultUntil(3*time.Second,
			func() (bool, error) {
				got := statusesByName(shim.List(alloc.ID))
				for name, status := range exp {
					if got[name] != status {
						return false, fmt.Errorf("expected %v, got %v", exp, got)
					}
				}
				return true, nil
			},
			func(err error) { t.Fatal(err) },
		)
	}

	// results carry the role of their check
	roles := make(map[string]string)
	for _, result := range shim.List(alloc.ID) {
		roles[result.Check] = result.Role
	}
	must.Eq(t, map[string]string{
		"legacy":  "",
		"alive":   structs.CheckRoleLiveness,
		"ready":   structs.CheckRoleReadiness,
		"started": structs.CheckRoleStartup,
	}, roles)

	// until the startup check passes, readiness and liveness checks do not
	// run, while checks without a role run as always
	waitFor(map[string]structs.CheckStatus{
		"started": structs.CheckFailure,
		"legacy":  structs.CheckFailure,
	})
	time.Sleep(300 * time.Millisecond)
	waitFor(map[string]structs.CheckStatus{
		"alive": structs.CheckPending,
		"ready": structs.CheckPending,
	})

	// startup passes, the gate opens
	started.Store(true)
	waitFor(map[string]structs.CheckStatus{
		"started": structs.CheckSuccess,
		"alive":   structs.CheckSuccess,
		"ready":   structs.CheckSuccess,
	})

	// the workload restarts: its role check results are reset, which closes
	// the gate again until the startup check passes
	started.Store(false)
	var ids []structs.CheckID
	for id, result := range shim.List(alloc.ID) {
		if result.Role != "" {
			ids = append(ids, id)
		}
	}
	must.NoError(t, shim.Reset(alloc.ID, ids))
	waitFor(map[string]structs.CheckStatus{"started": structs.CheckFailure})
	time.Sleep(300 * time.Millisecond)
	waitFor(map[string]structs.CheckStatus{
		"alive": structs.CheckPending,
		"ready": structs.CheckPending,
	})
}

func TestCheckHook_Roles_ConsulLiveness(t *testing.T) {
	ci.Parallel(t)

	logger := testlog.HCLogger(t)

	ts := httptest.NewServer(checkHandler)
	defer ts.Close()

	tokens := strings.Split(ts.URL, ":")
	addr, port := strings.TrimPrefix(tokens[1], "//"), tokens[2]

	shim := makeCheckStore(logger)
	alloc := allocWithRoleChecks(addr, port, structs.ServiceProviderConsul)
	envBuilder := taskenv.NewBuilder(mock.Node(), alloc, nil, alloc.Job.Region)

	h := newChecksHook(logger, alloc, shim, mock.NewNetworkStatus(addr), envBuilder.Build())
	must.NoError(t, h.Prerun())
	defer h.PreKill()

	// only the liveness check of the consul service is executed by the
	// client; there is no startup gate since consul runs the startup check
	testutil.WaitForResultUntil(3*time.Second,
		func() (bool, error) {
			got := statusesByName(shim.List(alloc.ID))
			if len(got) != 1 || got["alive"] != structs.CheckSuccess {
				return false, fmt.Errorf("expected only alive=success, got %v", got)
			}
			return true, nil
		},
		func(err error) { t.Fatal(err) },
	)

	// an update keeps the liveness observer
	must.NoError(t, h.Update(&interfaces.RunnerUpdateRequest{Alloc: alloc}))
	must.MapLen(t, 1, h.observers)
}

func TestCheckHook_clientCheckedServices(t *testing.T) {
	ci.Parallel(t)

	alloc := allocWithRoleChecks("127.0.0.1", "8080", structs.ServiceProviderConsul)
	group := alloc.Job.LookupTaskGroup(alloc.TaskGroup)
	group.Services = []*structs.Service{{
		Name:     "group-nomad",
		Provider: structs.ServiceProviderNomad,
		Checks:   []*structs.ServiceCheck{{Name: "g1"}, {Name: "g2", Role: structs.CheckRoleStartup}},
	}}

	services := clientCheckedServices(group)
	must.Len(t, 2, services)

	must.Eq(t, groupWorkload, services[0].workload)
	must.Len(t, 2, services[0].service.Checks)

	must.Eq(t, group.Tasks[0].Name, services[1].workload)
	must.Len(t, 1, services[1].service.Checks)
	must.Eq(t, "alive", services[1].service.Checks[0].Name)

	// the job is not modified
	must.Len(t, 4, group.Tasks[0].Services[0].Checks)
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func roleCheck(role string) *ServiceCheck {
	return &ServiceCheck{
		Name:     "check-" + role,
		Type:     ServiceCheckHTTP,
		Path:     "/health",
		Interval: 10 * time.Second,
		Timeout:  2 * time.Second,
		Role:     role,
	}
}

func TestServiceCheck_Role_Canonicalize(t *testing.T) {
	ci.Parallel(t)

	t.Run("no role", func(t *testing.T) {
		c := roleCheck("")
		c.Canonicalize("svc", "task")
		must.Zero(t, c.StartupTimeout)
		must.Nil(t, c.CheckRestart)
	})

	t.Run("startup default timeout", func(t *testing.T) {
		c := roleCheck(CheckRoleStartup)
		c.Canonicalize("svc", "task")
		must.Eq(t, DefaultStartupTimeout, c.StartupTimeout)
		must.Nil(t, c.CheckRestart)
	})

	t.Run("startup explicit timeout", func(t *testing.T) {
		c := roleCheck(CheckRoleStartup)
		c.StartupTimeout = 10 * time.Minute
		c.Canonicalize("svc", "task")
		must.Eq(t, 10*time.Minute, c.StartupTimeout)
	})

	t.Run("liveness default check_restart", func(t *testing.T) {
		c := roleCheck(CheckRoleLiveness)
		c.Canonicalize("svc", "task")
		must.Eq(t, &CheckRestart{Limit: DefaultLivenessRestartLimit}, c.CheckRestart)
	})

	t.Run("liveness explicit check_restart", func(t *testing.T) {
		c := roleCheck(CheckRoleLiveness)
		c.CheckRestart = &CheckRestart{Limit: 5, Grace: time.Second}
		c.Canonicalize("svc", "task")
		must.Eq(t, &CheckRestart{Limit: 5, Grace: time.Second}, c.CheckRestart)
	})

	t.Run("readiness", func(t *testing.T) {
		c := roleCheck(CheckRoleReadiness)
		c.Canonicalize("svc", "task")
		must.Zero(t, c.StartupTimeout)
		must.Nil(t, c.CheckRestart)
	})
}

func TestServiceCheck_Role_Validate(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		name   string
		check  func() *ServiceCheck
		expErr string
	}{
		{
			name:  "no role",
			check: func() *ServiceCheck { return roleCheck("") },
		},
		{
			name:  "startup",
			check: func() *ServiceCheck { c := roleCheck(CheckRoleStartup); c.StartupTimeout = time.Minute; return c },
		},
		{
			name:  "readiness",
			check: func() *ServiceCheck { return roleCheck(CheckRoleReadiness) },
		},
		{
			name: "liveness with check_restart",
			check: func() *ServiceCheck {
				c := roleCheck(CheckRoleLiveness)
				c.CheckRestart = &CheckRestart{Limit: 3}
				return c
			},
		},
		{
			name: "liveness with check_restart and on_update ignore",
			check: func() *ServiceCheck {
				c := roleCheck(CheckRoleLiveness)
				c.CheckRestart = &CheckRestart{Limit: 3}
				c.OnUpdate = OnUpdateIgnore
				return c
			},
		},
		{
			name:   "unknown role",
			check:  func() *ServiceCheck { return roleCheck("bogus") },
			expErr: `role must be "startup", "readiness", "liveness" or empty; got "bogus"`,
		},
		{
			name:   "negative startup_timeout",
			check:  func() *ServiceCheck { c := roleCheck(CheckRoleStartup); c.StartupTimeout = -time.Second; return c },
			expErr: "startup_timeout must be non-negative",
		},
		{
			name:   "startup_timeout without startup role",
			check:  func() *ServiceCheck { c := roleCheck(CheckRoleLiveness); c.StartupTimeout = time.Minute; return c },
			expErr: `startup_timeout may only be set on checks with role "startup"`,
		},
		{
			name: "readiness with check_restart",
			check: func() *ServiceCheck {
				c := roleCheck(CheckRoleReadiness)
				c.CheckRestart = &CheckRestart{Limit: 3}
				return c
			},
			expErr: `check_restart may not be set on checks with role "readiness"`,
		},
		{
			name: "startup with check_restart",
			check: func() *ServiceCheck {
				c := roleCheck(CheckRoleStartup)
				c.CheckRestart = &CheckRestart{Limit: 3}
				return c
			},
			expErr: `check_restart may not be set on checks with role "startup"`,
		},
		{
			name: "no role with check_restart and on_update ignore",
			check: func() *ServiceCheck {
				c := roleCheck("")
				c.CheckRestart = &CheckRestart{Limit: 3}
				c.OnUpdate = OnUpdateIgnore
				return c
			},
			expErr: `on_update value "ignore" is not compatible with check_restart`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// both providers validate roles the same way
			for _, validate := range []func(*ServiceCheck) error{
				(*ServiceCheck).validateNomad,
				(*ServiceCheck).validateConsul,
			} {
				err := validate(tc.check())
				if tc.expErr == "" {
					must.NoError(t, err)
				} else {
					must.ErrorContains(t, err, tc.expErr)
				}
			}
		})
	}
}

func TestServiceCheck_Role_validateConsulLiveness(t *testing.T) {
	ci.Parallel(t)

	// liveness checks of consul services are executed by the nomad client
	c := roleCheck(CheckRoleLiveness)
	c.Type = ServiceCheckGRPC
	must.ErrorContains(t, c.validateConsul(), "liveness checks are executed by the Nomad client")

	c = roleCheck(CheckRoleLiveness)
	c.TLSSkipVerify = true
	must.ErrorContains(t, c.validateConsul(), "liveness checks are executed by the Nomad client")

	// other roles keep the consul options
	c = roleCheck(CheckRoleReadiness)
	c.Type = ServiceCheckGRPC
	must.NoError(t, c.validateConsul())
}

func TestServiceCheck_Role_TriggersRestarts(t *testing.T) {
	ci.Parallel(t)

	withRestart := func(role string, limit int) *ServiceCheck {
		c := roleCheck(role)
		c.CheckRestart = &CheckRestart{Limit: limit}
		return c
	}

	must.False(t, roleCheck("").TriggersRestarts())
	must.True(t, withRestart("", 3).TriggersRestarts())
	must.False(t, withRestart("", 0).TriggersRestarts())
	must.True(t, roleCheck(CheckRoleStartup).TriggersRestarts())
	must.False(t, roleCheck(CheckRoleReadiness).TriggersRestarts())
	must.False(t, withRestart(CheckRoleReadiness, 3).TriggersRestarts())
	must.True(t, roleCheck(CheckRoleLiveness).TriggersRestarts())
	must.True(t, withRestart(CheckRoleLiveness, 3).TriggersRestarts())
	must.False(t, withRestart(CheckRoleLiveness, 0).TriggersRestarts())
}

func TestServiceCheck_Role_IDStability(t *testing.T) {
	ci.Parallel(t)

	// checks without a role keep the IDs they had before check roles
	c := roleCheck("")
	must.Eq(t, "75830179f724241054a36a0d2bb448bd1443f3b8", c.Hash("svc"))
	must.Eq(t, CheckID("f0acaae43c2da0debb104630e514ef06"), NomadCheckID("a", "g", c))

	noRole := roleCheck("")
	liveness := roleCheck(CheckRoleLiveness)
	liveness.Name = noRole.Name
	must.NotEq(t, noRole.Hash("svc"), liveness.Hash("svc"))
	must.NotEq(t, NomadCheckID("a", "g", noRole), NomadCheckID("a", "g", liveness))

	// startup_timeout does not change the consul check
	s1 := roleCheck(CheckRoleStartup)
	s2 := roleCheck(CheckRoleStartup)
	s2.StartupTimeout = time.Hour
	must.Eq(t, s1.Hash("svc"), s2.Hash("svc"))
}

func TestServiceCheck_Role_Equal(t *testing.T) {
	ci.Parallel(t)

	a := roleCheck(CheckRoleStartup)
	a.Path = "" // Equal compares PortLabel to Path
	b := a.Copy()
	must.True(t, a.Equal(b))

	b.Role = CheckRoleReadiness
	must.False(t, a.Equal(b))

	b = a.Copy()
	b.StartupTimeout = time.Minute
	must.False(t, a.Equal(b))
}

func TestServiceRegistration_Equal_Ready(t *testing.T) {
	ci.Parallel(t)

	a := &ServiceRegistration{ID: "id", ServiceName: "svc", Ready: true}
	b := a.Copy()
	must.True(t, a.Equal(b))

	// a change of readiness must reach the state store
	b.Ready = false
	must.False(t, a.Equal(b))
}

func TestTaskGroup_validateCheckRoles(t *testing.T) {
	ci.Parallel(t)

	service := func(name string, checks ...*ServiceCheck) *Service {
		return &Service{Name: name, Checks: checks}
	}

	tg := &TaskGroup{
		Services: []*Service{service("g1", roleCheck(CheckRoleStartup))},
		Tasks: []*Task{
			{Name: "t1", Services: []*Service{
				service("s1", roleCheck(CheckRoleStartup), roleCheck(CheckRoleLiveness)),
				service("s2", roleCheck(CheckRoleReadiness)),
			}},
			{Name: "t2", Services: []*Service{
				service("s3", roleCheck(CheckRoleStartup)),
			}},
		},
	}
	must.NoError(t, tg.validateCheckRoles())

	// a second startup check in another service of the same task
	tg.Tasks[0].Services[1].Checks = append(tg.Tasks[0].Services[1].Checks, roleCheck(CheckRoleStartup))
	must.ErrorContains(t, tg.validateCheckRoles(), `Task t1 may have at most one check with role "startup", found 2`)

	// a second startup check among group services
	tg.Tasks[0].Services[1].Checks = tg.Tasks[0].Services[1].Checks[:1]
	tg.Services = append(tg.Services, service("g2", roleCheck(CheckRoleStartup)))
	must.ErrorContains(t, tg.validateCheckRoles(), `Group services may have at most one check with role "startup", found 2`)
}

func TestTaskGroup_checkRoleWarnings(t *testing.T) {
	ci.Parallel(t)

	ignored := func(role string) *ServiceCheck {
		c := roleCheck(role)
		c.OnUpdate = OnUpdateIgnore
		return c
	}

	tg := &TaskGroup{
		Tasks: []*Task{{Name: "t1", Services: []*Service{{
			Name:   "s1",
			Checks: []*ServiceCheck{ignored(""), ignored(CheckRoleLiveness), roleCheck(CheckRoleReadiness)},
		}}}},
	}
	must.NoError(t, tg.checkRoleWarnings())

	tg.Tasks[0].Services[0].Checks = append(tg.Tasks[0].Services[0].Checks, ignored(CheckRoleReadiness))
	err := tg.checkRoleWarnings()
	must.ErrorContains(t, err, `check "check-readiness" has role "readiness", but on_update = "ignore"`)
}

func TestServiceCheck_Role_Diff(t *testing.T) {
	ci.Parallel(t)

	old := roleCheck("")
	new := roleCheck(CheckRoleStartup)
	new.Name = old.Name
	new.StartupTimeout = 10 * time.Minute

	diff := serviceCheckDiff(old, new, false)
	must.NotNil(t, diff)
	must.Eq(t, DiffTypeEdited, diff.Type)
	must.Eq(t, []*FieldDiff{
		{Type: DiffTypeAdded, Name: "Role", Old: "", New: CheckRoleStartup},
		{Type: DiffTypeEdited, Name: "StartupTimeout", Old: "0", New: "600000000000"},
	}, diff.Fields)
}

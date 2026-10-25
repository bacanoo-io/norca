// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/api/mtls"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// mtlsTestGroup returns a task group running a long mock task with a native
// mTLS service.
func mtlsTestGroup(name string) *api.TaskGroup {
	return &api.TaskGroup{
		Name:  pointer.Of(name),
		Count: pointer.Of(1),
		Networks: []*api.NetworkResource{{
			DynamicPorts: []api.Port{{Label: "tls"}},
		}},
		Services: []*api.Service{{
			Name:      name,
			PortLabel: "tls",
			Provider:  "nomad",
			MTLS:      &api.ServiceMTLS{Mode: api.MTLSModeApp},
		}},
		Tasks: []*api.Task{{
			Name:   "app",
			Driver: "mock_driver",
			Config: map[string]any{"run_for": "5m"},
			Resources: &api.Resources{
				CPU:      pointer.Of(100),
				MemoryMB: pointer.Of(64),
			},
		}},
	}
}

// TestMTLS_Integration runs two tasks in "app" mode: both get workload
// certificates from the region's CA, a mutual TLS connection between them
// works while an intention allows it, and the library rejects it once a deny
// intention takes precedence.
func TestMTLS_Integration(t *testing.T) {
	ci.Parallel(t)

	httpTest(t, nil, func(s *TestAgent) {
		client := s.Client()

		job := &api.Job{
			ID:          pointer.Of("shop"),
			Name:        pointer.Of("shop"),
			Type:        pointer.Of("service"),
			Datacenters: []string{"dc1"},
			TaskGroups:  []*api.TaskGroup{mtlsTestGroup("web"), mtlsTestGroup("api")},
		}
		_, _, err := client.Jobs().Register(job, nil)
		must.NoError(t, err)

		// Both tasks run and have their certificate, key and bundle.
		allocDir := s.client.GetConfig().AllocDir
		dirs := map[string]string{}
		must.Wait(t, wait.InitialSuccess(
			wait.ErrorFunc(func() error {
				allocs, _, err := client.Jobs().Allocations("shop", false, nil)
				if err != nil {
					return err
				}
				for _, alloc := range allocs {
					if alloc.ClientStatus != api.AllocClientStatusRunning {
						return fmt.Errorf("alloc %s is %s", alloc.ID, alloc.ClientStatus)
					}
					dir := filepath.Join(allocDir, alloc.ID, "app", "secrets", mtls.Dir)
					for _, f := range []string{mtls.CertFile, mtls.KeyFile, mtls.CAFile} {
						if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
							return err
						}
					}
					dirs[alloc.TaskGroup] = dir
				}
				if len(dirs) != 2 {
					return fmt.Errorf("%d allocations", len(dirs))
				}
				return nil
			}),
			wait.Timeout(60*time.Second),
			wait.Gap(250*time.Millisecond),
		))

		source := func(group string) *mtls.FileSource {
			dir := dirs[group]
			src, err := mtls.NewFileSource(filepath.Join(dir, mtls.CertFile), filepath.Join(dir, mtls.KeyFile), filepath.Join(dir, mtls.CAFile))
			must.NoError(t, err)
			return src
		}
		web, apiSrc := source("web"), source("api")
		must.Eq(t, mtls.ID{TrustDomain: "global.nomad", Namespace: "default", Job: "shop", Group: "web"}, web.ID())
		must.Eq(t, []string{"api"}, apiSrc.Services())

		// The api task enforces the intentions of its namespace, which it
		// follows with blocking queries.
		_, err = client.ServiceIntentions().Upsert(&api.ServiceIntention{
			Source: "web", Destination: "api", Action: api.ServiceIntentionActionAllow}, nil)
		must.NoError(t, err)
		watcher := mtls.NewIntentionWatcher(client, "default")
		watcher.WaitTime = 2 * time.Second
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go watcher.Run(ctx)
		readyCtx, readyCancel := context.WithTimeout(ctx, 10*time.Second)
		defer readyCancel()
		must.NoError(t, watcher.WaitReady(readyCtx))

		authz := &mtls.Authorizer{Namespace: "default", Intentions: watcher}
		ln, err := tls.Listen("tcp", "127.0.0.1:0", apiSrc.ServerConfig(authz, "api"))
		must.NoError(t, err)
		defer ln.Close()
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer conn.Close()
					line, err := bufio.NewReader(conn).ReadString('\n')
					if err == nil {
						_, _ = conn.Write([]byte("pong " + line))
					}
				}()
			}
		}()

		call := func() error {
			conn, err := tls.Dial("tcp", ln.Addr().String(), web.ClientConfig(mtls.ExpectService("default", "api")))
			if err != nil {
				return err
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := conn.Write([]byte("ping\n")); err != nil {
				return err
			}
			line, err := bufio.NewReader(conn).ReadString('\n')
			if err != nil {
				return err
			}
			if line != "pong ping\n" {
				return errors.New("unexpected reply " + line)
			}
			return nil
		}
		must.NoError(t, call())

		// A deny intention for every source of the namespace does not
		// override the more specific allow; a deny for web does.
		_, err = client.ServiceIntentions().Upsert(&api.ServiceIntention{
			Source: "*", Destination: "api", Action: api.ServiceIntentionActionDeny}, nil)
		must.NoError(t, err)
		_, err = client.ServiceIntentions().Upsert(&api.ServiceIntention{
			Source: "web", Destination: "api", Action: api.ServiceIntentionActionDeny}, nil)
		must.NoError(t, err)
		must.Wait(t, wait.InitialSuccess(
			wait.BoolFunc(func() bool {
				d := mtls.Evaluate(watcher.Intentions(), mtls.Source{Namespace: "default", Services: []string{"web"}}, "default", "api", false)
				return !d.Allowed && d.Intention != nil && d.Intention.Source == "web"
			}),
			wait.Timeout(10*time.Second),
			wait.Gap(100*time.Millisecond),
		))
		must.Error(t, call())
		must.ErrorIs(t, authz.Authorize(&mtls.Peer{ID: web.ID(), Services: web.Services()}, "api"), mtls.ErrDenied)

		// The trust bundle of the HTTP API holds the CA that signed them.
		roots, _, err := client.MTLS().Roots(nil)
		must.NoError(t, err)
		must.Len(t, 1, roots.Roots)
		must.Eq(t, "global.nomad", roots.TrustDomain)
	})
}

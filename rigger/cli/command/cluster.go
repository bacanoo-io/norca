// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"log/slog"
	"os"

	"github.com/bacanoo-io/norca/norca/api"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/cli/norcaapi"
	"github.com/bacanoo-io/norca/rigger/fetch"
)

// newClient returns a client of the Norca API configured as the norca
// command is: NORCA_ADDR, NORCA_TOKEN, NORCA_REGION and the TLS settings
// of the environment. A region given here wins.
func newClient(region string) (*api.Client, error) {
	cfg := api.DefaultConfig()
	if region != "" {
		cfg.Region = region
	}

	return api.NewClient(cfg)
}

// clusterEngine returns an engine with the ports over the cluster of the
// client; secrets live in namespace.
func clusterEngine(logger *slog.Logger, client *api.Client, namespace string) (*rigger.Engine, error) {
	fetcher, err := newFetcher()
	if err != nil {
		return nil, err
	}

	return rigger.New(
		rigger.WithLogger(logger),
		rigger.WithFetcher(fetcher),
		rigger.WithJobDecoder(norcaapi.JobDecoder{}), rigger.WithResourceDecoder(norcaapi.ResourceDecoder{}),
		rigger.WithCluster(norcaapi.NewCluster(client)),
		rigger.WithReleases(norcaapi.NewReleases(client)),
		rigger.WithSecrets(norcaapi.NewSecrets(client, namespace)),
	)
}

// newFetcher returns the fetcher of modules with its cache (RIGGER_CACHE_DIR)
// and the global registry (RIGGER_GLOBAL_REGISTRY, a git address or a
// directory).
func newFetcher() (*fetch.Fetcher, error) {
	dir, err := fetch.CacheDir()
	if err != nil {
		return nil, err
	}

	f := fetch.New(dir)
	f.SetCredentials(gitCredentials)

	if global := os.Getenv("RIGGER_GLOBAL_REGISTRY"); global != "" {
		f.SetGlobalRegistry(global)
	}

	return f, nil
}

// gitCredentials are the credentials of the rigger program for git
// repositories (the fetcher reads them with go-git, not with the git
// program, so the credential helpers and ~/.ssh/config of git do not
// apply): an ssh URL and user@host:path authenticate with the keys of the
// ssh agent and the known hosts of the user; an https URL with
// RIGGER_GIT_PASSWORD (a token) and RIGGER_GIT_USERNAME when they are set,
// else anonymously.
func gitCredentials(_ context.Context, repository string) (*fetch.Credentials, error) {
	if fetch.SSHRepository(repository) {
		return &fetch.Credentials{SSHAgent: true}, nil
	}

	if password := os.Getenv("RIGGER_GIT_PASSWORD"); password != "" {
		return &fetch.Credentials{Username: os.Getenv("RIGGER_GIT_USERNAME"), Password: password}, nil
	}

	return nil, nil
}

// offlineEngine is an engine without a cluster: it loads, fetches and
// evaluates.
func offlineEngine(logger *slog.Logger) (*rigger.Engine, error) {
	fetcher, err := newFetcher()
	if err != nil {
		return nil, err
	}

	return rigger.New(rigger.WithLogger(logger), rigger.WithJobDecoder(norcaapi.JobDecoder{}), rigger.WithResourceDecoder(norcaapi.ResourceDecoder{}), rigger.WithFetcher(fetcher))
}

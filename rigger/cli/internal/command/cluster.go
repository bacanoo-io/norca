// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"log/slog"
	"os"

	"github.com/bacanoo-io/norca/norca/api"

	rig "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/cli/internal/fetch"
	"github.com/bacanoo-io/norca/rigger/cli/norcaapi"
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
func clusterEngine(logger *slog.Logger, client *api.Client, namespace string) (*rig.Engine, error) {
	fetcher, err := newFetcher()
	if err != nil {
		return nil, err
	}

	return rig.New(
		rig.WithLogger(logger),
		rig.WithFetcher(fetcher),
		rig.WithJobDecoder(norcaapi.JobDecoder{}), rig.WithResourceDecoder(norcaapi.ResourceDecoder{}),
		rig.WithCluster(norcaapi.NewCluster(client)),
		rig.WithReleases(norcaapi.NewReleases(client)),
		rig.WithSecrets(norcaapi.NewSecrets(client, namespace)),
	)
}

// newFetcher returns the fetcher of modules with its cache (RIG_CACHE_DIR)
// and the global registry (RIG_GLOBAL_REGISTRY, a git address or a
// directory).
func newFetcher() (*fetch.Fetcher, error) {
	dir, err := fetch.CacheDir()
	if err != nil {
		return nil, err
	}

	f := fetch.New(dir)

	if global := os.Getenv("RIG_GLOBAL_REGISTRY"); global != "" {
		f.SetGlobalRegistry(global)
	}

	return f, nil
}

// offlineEngine is an engine without a cluster: it loads, fetches and
// evaluates.
func offlineEngine(logger *slog.Logger) (*rig.Engine, error) {
	fetcher, err := newFetcher()
	if err != nil {
		return nil, err
	}

	return rig.New(rig.WithLogger(logger), rig.WithJobDecoder(norcaapi.JobDecoder{}), rig.WithResourceDecoder(norcaapi.ResourceDecoder{}), rig.WithFetcher(fetcher))
}

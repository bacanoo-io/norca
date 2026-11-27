// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"log/slog"
	"os"

	"github.com/bacanoo-io/norca/norca/api"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/cli/internal/credstore"
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

// queryOptions and writeOptions are the options of a request of the API that
// ends when ctx is done.
func queryOptions(ctx context.Context) *api.QueryOptions {
	return (&api.QueryOptions{}).WithContext(ctx)
}

func writeOptions(ctx context.Context) *api.WriteOptions {
	return (&api.WriteOptions{}).WithContext(ctx)
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
		rigger.WithJobDecoder(norcaapi.JobDecoder{}),
		rigger.WithResourceDecoder(norcaapi.ResourceDecoder{}),
		rigger.WithKinds(norcaapi.Kinds()...),
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
	f.SetOCICredentials(credentials{kind: credstore.OCI}.oci)

	if global := os.Getenv("RIGGER_GLOBAL_REGISTRY"); global != "" {
		f.SetGlobalRegistry(global)
	}

	return f, nil
}

// offlineEngine is an engine without a cluster: it loads, fetches and
// evaluates.
func offlineEngine(logger *slog.Logger) (*rigger.Engine, error) {
	fetcher, err := newFetcher()
	if err != nil {
		return nil, err
	}

	return rigger.New(
		rigger.WithLogger(logger),
		rigger.WithJobDecoder(norcaapi.JobDecoder{}),
		rigger.WithResourceDecoder(norcaapi.ResourceDecoder{}),
		rigger.WithKinds(norcaapi.Kinds()...),
		rigger.WithFetcher(fetcher),
	)
}

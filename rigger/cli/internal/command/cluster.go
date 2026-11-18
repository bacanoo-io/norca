// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"log/slog"

	"github.com/bacanoo-io/norca/norca/api"

	rig "github.com/bacanoo-io/norca-rig"
	"github.com/bacanoo-io/norca-rig/cli/norcaapi"
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
	return rig.New(
		rig.WithLogger(logger),
		rig.WithJobDecoder(norcaapi.JobDecoder{}),
		rig.WithCluster(norcaapi.NewCluster(client)),
		rig.WithReleases(norcaapi.NewReleases(client)),
		rig.WithSecrets(norcaapi.NewSecrets(client, namespace)),
	)
}

// jobDecoder is the job decoder of Norca.
func jobDecoder() norcaapi.JobDecoder { return norcaapi.JobDecoder{} }

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/client/secrets"
	"github.com/hashicorp/nomad/client/vaultcompat"
	"github.com/hashicorp/nomad/nomad/structs"
)

// setupVaultCompat sets up the Vault-compatible endpoint of the built-in
// secret store (norca-doc content/docs/design/secrets-plan.md, "Vault compatibility") when the
// agent has no Vault integration, and makes its client the Vault client of
// the task runners: tasks with a vault block get a token of the endpoint.
func (c *Client) setupVaultCompat() error {
	cfg := c.GetConfig()
	if !cfg.VaultCompatEnabled() {
		return nil
	}
	var compat *config.VaultCompatConfig
	var rules []*config.VaultKVRule
	var routes []*config.SecretsRoute
	if cfg.Secrets != nil {
		compat, rules, routes = cfg.Secrets.VaultCompat, cfg.Secrets.VaultKV, cfg.Secrets.Routes
	}
	limit, burst := compat.Rate()

	server := vaultcompat.New(vaultcompat.Config{
		Logger: c.logger,
		Reader: &secrets.VaultReader{
			Registry: secrets.NewRegistry(secrets.NewNorcaProvider(c, cfg.Region), nil, routes),
			Mapper:   secrets.NewVaultMapper(rules),
			Auditor:  secrets.NewRPCAuditor(c, cfg.Region),
		},
		NodeSecret:   c.secretNodeID(),
		Port:         compat.PortOrDefault(),
		Bridge:       compat.BridgeEnabled(),
		BridgeSubnet: cfg.BridgeNetworkAllocSubnet,
		TokenTTL:     compat.TokenTTLOrDefault(),
		RateLimit:    limit,
		RateBurst:    burst,
		AllocInfo:    c.vaultCompatAllocInfo,
	})
	// The endpoint listens once the first task with a vault block asks for
	// a token.
	c.vaultCompat = server
	c.vaultClient = server.Client()
	return nil
}

// vaultCompatAllocInfo reports about a task of an allocation of the client
// to the Vault-compatible endpoint: the allocation, whether the task still
// runs, and the allocation's address in the bridge network.
func (c *Client) vaultCompatAllocInfo(allocID, task string) (*structs.Allocation, bool, string) {
	ar, err := c.getAllocRunner(allocID)
	if err != nil {
		return nil, false, ""
	}
	alloc := ar.Alloc()
	if alloc == nil || alloc.ClientTerminalStatus() || ar.IsDestroyed() {
		return alloc, false, ""
	}
	state := ar.AllocState()
	if state != nil {
		if ts := state.TaskStates[task]; ts != nil && ts.State == structs.TaskStateDead {
			return alloc, false, ""
		}
	}
	bridgeIP := ""
	if tg := alloc.Job.LookupTaskGroup(alloc.TaskGroup); tg != nil && len(tg.Networks) > 0 &&
		tg.Networks[0].Mode == "bridge" && state != nil && state.NetworkStatus != nil {
		bridgeIP = state.NetworkStatus.Address
	}
	return alloc, true, bridgeIP
}

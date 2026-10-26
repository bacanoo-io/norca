// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package vaultcompat

import (
	"errors"
	"fmt"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/hashicorp/nomad/client/vaultclient"
	"github.com/hashicorp/nomad/nomad/structs"
)

// Client is the Vault client of the task runners when the agent has no
// Vault integration: it gives the tasks with a vault block tokens of the
// endpoint instead of Vault tokens. The vault hook works with it unchanged:
// it writes the token to the task's secrets directory and environment,
// "renews" it (the client keeps it alive while the task runs) and stops the
// renewal when the task stops, which revokes the token.
type Client struct {
	s *Server
}

var _ vaultclient.VaultClient = (*Client)(nil)

// Client returns the Vault client of the endpoint.
func (s *Server) Client() *Client { return &Client{s: s} }

// VaultAddr returns the VAULT_ADDR of a task of the allocation.
func (c *Client) VaultAddr(alloc *structs.Allocation) string { return c.s.VaultAddr(alloc) }

func (c *Client) Start() {}
func (c *Client) Stop()  {}

// DeriveToken returns the tokens of tasks of the allocation.
func (c *Client) DeriveToken(alloc *structs.Allocation, tasks []string) (map[string]string, error) {
	if alloc == nil || alloc.Job == nil {
		return nil, errors.New("missing allocation")
	}
	tg := alloc.Job.LookupTaskGroup(alloc.TaskGroup)
	if tg == nil {
		return nil, fmt.Errorf("task group %q not found", alloc.TaskGroup)
	}
	if err := c.s.Start(); err != nil {
		return nil, structs.NewRecoverableError(err, true)
	}
	if usesBridge(alloc) {
		if err := c.s.ensureBridge(); err != nil {
			// The bridge appears with the first allocation network.
			return nil, structs.NewRecoverableError(err, true)
		}
	}
	out := make(map[string]string, len(tasks))
	for _, name := range tasks {
		task := tg.LookupTask(name)
		if task == nil || task.Vault == nil {
			return nil, fmt.Errorf("task %q has no vault block", name)
		}
		identity := alloc.SignedIdentities[name]
		if identity == "" {
			return nil, fmt.Errorf("task %q has no workload identity to read secrets with", name)
		}
		out[name] = c.s.tokens.derive(taskInfo(alloc, task, identity))
	}
	return out, nil
}

func taskInfo(alloc *structs.Allocation, task *structs.Task, identity string) *TaskInfo {
	info := &TaskInfo{
		AllocID:   alloc.ID,
		Namespace: alloc.Namespace,
		JobID:     alloc.JobID,
		Group:     alloc.TaskGroup,
		Task:      task.Name,
		Job:       alloc.Job,
		Identity:  identity,
	}
	if task.Vault != nil {
		info.Policies = append([]string(nil), task.Vault.Policies...)
	}
	return info
}

// adopt returns the task of a token recovered after an agent restart, if
// the task still runs on this client.
func (s *Server) adopt(allocID, taskName string) (*TaskInfo, bool) {
	if s.cfg.AllocInfo == nil {
		return nil, false
	}
	alloc, live, _ := s.cfg.AllocInfo(allocID, taskName)
	if alloc == nil || !live || alloc.Job == nil {
		return nil, false
	}
	tg := alloc.Job.LookupTaskGroup(alloc.TaskGroup)
	if tg == nil {
		return nil, false
	}
	task := tg.LookupTask(taskName)
	identity := alloc.SignedIdentities[taskName]
	if task == nil || task.Vault == nil || identity == "" {
		return nil, false
	}
	if usesBridge(alloc) && s.ensureBridge() != nil {
		return nil, false
	}
	return taskInfo(alloc, task, identity), true
}

// GetConsulACL is not supported: there is no Vault to read Consul tokens
// from.
func (c *Client) GetConsulACL(string, string) (*vaultapi.Secret, error) {
	return nil, errors.New("Consul ACL tokens from Vault are not supported by the built-in secret store")
}

// RenewToken keeps the token alive while the task runs; a token recovered
// after an agent restart is adopted again if its task still runs. The
// returned channel never reports an error: the token stays valid until the
// renewal is stopped.
func (c *Client) RenewToken(token string, _ int) (<-chan error, error) {
	if err := c.s.Start(); err != nil {
		return nil, err
	}
	if err := c.s.tokens.track(token, c.s.adopt); err != nil {
		return nil, err
	}
	return make(chan error, 1), nil
}

// StopRenewToken revokes the token: its task stopped.
func (c *Client) StopRenewToken(token string) error {
	c.s.tokens.revoke(token)
	return nil
}

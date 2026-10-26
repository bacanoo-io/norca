// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/hashicorp/nomad/nomad/structs"
	structsc "github.com/hashicorp/nomad/nomad/structs/config"
)

// vaultPollInterval is how often the vault provider reads a secret again to
// find changes: KV reads have no blocking queries.
var vaultPollInterval = 5 * time.Second

// VaultProvider is the agent's existing Vault integration as a provider. It
// reads KV secrets with the task's server derived Vault token (the task
// needs a vault block, exactly as for Vault template calls), from the Vault
// of the agent's vault block. Nothing about the integration itself changes.
type VaultProvider struct {
	config *structsc.VaultConfig

	lock   sync.Mutex
	client *vaultapi.Client
}

// NewVaultProvider returns the provider of the agent's Vault integration,
// or nil if it is not enabled.
func NewVaultProvider(config *structsc.VaultConfig) *VaultProvider {
	if config == nil || !config.IsEnabled() {
		return nil
	}
	return &VaultProvider{config: config}
}

func (p *VaultProvider) Name() string { return structs.SecretProviderVault }
func (p *VaultProvider) Type() string { return structs.SecretProviderVault }

func (p *VaultProvider) Capabilities() Capabilities {
	return Capabilities{Versions: true, PollInterval: vaultPollInterval}
}

func (p *VaultProvider) apiClient(id *Identity) (*vaultapi.Client, error) {
	p.lock.Lock()
	defer p.lock.Unlock()
	if p.client == nil {
		conf, err := p.config.ApiConfig()
		if err != nil {
			return nil, err
		}
		c, err := vaultapi.NewClient(conf)
		if err != nil {
			return nil, err
		}
		c.SetToken("")
		p.client = c
	}
	c, err := p.client.Clone()
	if err != nil {
		return nil, err
	}
	c.SetToken(id.VaultToken)
	ns := p.config.Namespace
	if id.VaultNamespace != "" {
		ns = id.VaultNamespace
	}
	if ns != "" {
		c.SetNamespace(ns)
	}
	return c, nil
}

// Fetch reads a KV secret. A KV v2 response (data and metadata) yields the
// items of data.data and the version of data.metadata; a KV v1 response
// yields its data.
func (p *VaultProvider) Fetch(ctx context.Context, req *Request) (*Secret, error) {
	if req.Engine != "" || req.Role != "" {
		return nil, NewError(p.Name(), KindInvalid, errors.New("dynamic secrets are not supported yet"))
	}
	if req.Identity == nil || req.Identity.VaultToken == "" {
		return nil, NewError(p.Name(), KindInvalid,
			errors.New("the vault provider reads with the task's Vault token: the task needs a vault block"))
	}
	c, err := p.apiClient(req.Identity)
	if err != nil {
		return nil, NewError(p.Name(), KindInvalid, err)
	}
	var data map[string][]string
	if req.Version != 0 {
		data = map[string][]string{"version": {strconv.FormatUint(req.Version, 10)}}
	}
	sec, err := c.Logical().ReadWithDataWithContext(ctx, req.Path, data)
	if err != nil {
		return nil, p.classify(err)
	}
	if sec == nil || sec.Data == nil {
		return nil, NewError(p.Name(), KindNotFound, fmt.Errorf("secret %q not found", req.Path))
	}
	return vaultSecret(sec.Data)
}

func vaultSecret(data map[string]any) (*Secret, error) {
	out := &Secret{Items: map[string]string{}}
	values := data
	inner, hasData := data["data"].(map[string]any)
	meta, hasMeta := data["metadata"].(map[string]any)
	if hasData && hasMeta {
		values = inner
		switch v := meta["version"].(type) {
		case json.Number:
			out.Version = v.String()
		case float64:
			out.Version = strconv.FormatFloat(v, 'f', -1, 64)
		case string:
			out.Version = v
		}
	} else if _, ok := data["data"]; ok && data["data"] == nil {
		// KV v2 response of a deleted version.
		return nil, NewError(structs.SecretProviderVault, KindNotFound, errors.New("secret version is deleted"))
	}
	for k, v := range values {
		switch t := v.(type) {
		case string:
			out.Items[k] = t
		case nil:
			out.Items[k] = ""
		default:
			b, err := json.Marshal(t)
			if err != nil {
				return nil, NewError(structs.SecretProviderVault, KindInvalid, fmt.Errorf("item %q: %v", k, err))
			}
			out.Items[k] = string(b)
		}
	}
	// KV v1 has no versions: the Version stays empty and changes are found
	// by comparing the values (Fingerprint).
	return out, nil
}

func (p *VaultProvider) classify(err error) error {
	var rerr *vaultapi.ResponseError
	if errors.As(err, &rerr) {
		switch rerr.StatusCode {
		case http.StatusForbidden, http.StatusUnauthorized:
			return NewError(p.Name(), KindDenied, errors.New("permission denied"))
		case http.StatusNotFound:
			return NewError(p.Name(), KindNotFound, errors.New("secret not found"))
		case http.StatusBadRequest:
			return NewError(p.Name(), KindInvalid, fmt.Errorf("bad request (status %d)", rerr.StatusCode))
		}
		return NewError(p.Name(), KindUnavailable, fmt.Errorf("vault returned status %d", rerr.StatusCode))
	}
	return NewError(p.Name(), KindUnavailable, err)
}

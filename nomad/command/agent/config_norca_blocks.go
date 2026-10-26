// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"

	clientconfig "github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/nomad/autoscaler"
	"github.com/hashicorp/nomad/nomad/backup"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/hashicorp/nomad/secretstore"
)

// norcaServerBlocks are the Norca blocks of the server configuration,
// converted and validated.
type norcaServerBlocks struct {
	admission    *config.AdmissionConfig
	keyring      *config.KeyringConfig
	mtls         *config.MTLSConfig
	autoscaler   *autoscaler.Config
	backup       *backup.Config
	secretsAudit *secretstore.AuditFileConfig

	// discoveryOverflowFraction is the default overflow fraction of the
	// native_service_discovery block; nil keeps the server default.
	discoveryOverflowFraction *float64
}

// convertNorcaServerBlocks converts and validates the Norca blocks of the
// server configuration: admission, keyring, mtls, autoscaler, backup, secrets
// and native_service_discovery. The agent runs it when it starts the server
// and config validate runs it on the files, so both report the same errors.
func convertNorcaServerBlocks(s *ServerConfig) (*norcaServerBlocks, error) {
	out := &norcaServerBlocks{}
	if s == nil {
		s = &ServerConfig{}
	}

	if admissionConf := s.Admission.Copy(); admissionConf != nil {
		admissionConf.Canonicalize()
		if err := admissionConf.Validate(); err != nil {
			return nil, fmt.Errorf("invalid server admission configuration: %w", err)
		}
		out.admission = admissionConf
	}

	if keyringConf := s.Keyring.Copy(); keyringConf != nil {
		if err := keyringConf.Validate(); err != nil {
			return nil, fmt.Errorf("invalid server keyring configuration: %w", err)
		}
		out.keyring = keyringConf
	}

	if mtlsConf := s.MTLS.Copy(); mtlsConf != nil {
		if err := mtlsConf.Finalize(); err != nil {
			return nil, fmt.Errorf("invalid server mtls configuration: %w", err)
		}
		if mtlsConf.TrustDomain != "" {
			if err := structs.ValidateSPIFFETrustDomain(mtlsConf.TrustDomain); err != nil {
				return nil, fmt.Errorf("invalid server mtls configuration: %w", err)
			}
		}
		out.mtls = mtlsConf
	}

	if err := s.NativeServiceDiscovery.Validate(); err != nil {
		return nil, fmt.Errorf("invalid server native_service_discovery configuration: %w", err)
	}
	if s.NativeServiceDiscovery != nil {
		out.discoveryOverflowFraction = s.NativeServiceDiscovery.DefaultOverflowFraction
	}

	var err error
	if out.autoscaler, err = s.Autoscaler.toServerConfig(); err != nil {
		return nil, err
	}
	if out.backup, err = s.Backup.toServerConfig(); err != nil {
		return nil, err
	}
	if out.secretsAudit, err = s.Secrets.toAuditFileConfig(); err != nil {
		return nil, err
	}
	return out, nil
}

// convertNorcaClientBlocks validates the Norca blocks of the client
// configuration (dns, consul_compat, secrets with vault_compat) and returns
// the client's secrets configuration. vault is the agent's vault block: the
// vault secret provider needs it enabled. The agent runs it when it starts
// the client and config validate runs it on the files.
func convertNorcaClientBlocks(c *ClientConfig, vault *config.VaultConfig) (*clientconfig.SecretsConfig, error) {
	if c == nil {
		return nil, nil
	}
	if err := c.DNS.Validate(); err != nil {
		return nil, fmt.Errorf("invalid dns config: %v", err)
	}
	if err := c.ConsulCompat.Validate(); err != nil {
		return nil, fmt.Errorf("invalid consul_compat config: %v", err)
	}
	secretsConf, err := c.Secrets.toClientConfig()
	if err != nil {
		return nil, fmt.Errorf("invalid client secrets config: %v", err)
	}
	if err := secretsConf.Validate(func(name string) bool {
		return name == structs.SecretProviderNorca ||
			(name == structs.SecretProviderVault && vault != nil && vault.IsEnabled())
	}); err != nil {
		return nil, fmt.Errorf("invalid client secrets config: %v", err)
	}
	return secretsConf, nil
}

// validateNorcaBlocks runs the value checks of the Norca blocks the agent
// runs when it starts, for the enabled server and client.
func validateNorcaBlocks(c *Config) error {
	if c.Server != nil && c.Server.Enabled {
		if _, err := convertNorcaServerBlocks(c.Server); err != nil {
			return err
		}
	}
	if c.Client != nil && c.Client.Enabled {
		if _, err := convertNorcaClientBlocks(c.Client, c.Vault); err != nil {
			return err
		}
	}
	return nil
}

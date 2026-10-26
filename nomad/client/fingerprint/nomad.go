// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package fingerprint

import (
	"strconv"

	log "github.com/hashicorp/go-hclog"

	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/version"
)

// NomadFingerprint is used to fingerprint the Nomad version
type NomadFingerprint struct {
	StaticFingerprinter
	logger log.Logger
}

// NewNomadFingerprint is used to create a Nomad fingerprint
func NewNomadFingerprint(logger log.Logger) Fingerprint {
	f := &NomadFingerprint{logger: logger.Named("nomad")}
	return f
}

func (f *NomadFingerprint) Fingerprint(req *FingerprintRequest, resp *FingerprintResponse) error {
	resp.AddAttribute("nomad.advertise.address", req.Node.HTTPAddr)
	resp.AddAttribute("nomad.version", req.Config.Version.VersionNumber())
	// norca.version lets servers tell Norca clients apart from upstream Nomad
	// clients, which report the same nomad.version.
	resp.AddAttribute("norca.version", version.NorcaVersion)
	resp.AddAttribute("nomad.revision", req.Config.Version.Revision)
	resp.AddAttribute("nomad.service_discovery", strconv.FormatBool(req.Config.NomadServiceDiscovery))
	// Without Vault, the vault blocks of tasks are served by the built-in
	// secret store; jobs with vault blocks are constrained to such clients
	// when the servers have no Vault either (Norca).
	if req.Config.VaultCompatEnabled() {
		resp.AddAttribute(config.AttrVaultCompat, "true")
	}
	resp.Detected = true
	return nil
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fingerprint

import (
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/allocrunner/netpolicy"
	"github.com/hashicorp/nomad/nomad/structs"
)

// NetworkPolicyFingerprint detects whether the node can enforce network
// policies with nftables. The scheduler only places allocations selected by
// network policies with enforcement "required" on nodes with the
// norca.network_policy.nftables attribute.
type NetworkPolicyFingerprint struct {
	StaticFingerprinter
	logger   hclog.Logger
	detector func() error
}

func NewNetworkPolicyFingerprint(logger hclog.Logger) Fingerprint {
	return &NetworkPolicyFingerprint{
		logger:   logger.Named("network_policy"),
		detector: netpolicy.Supported,
	}
}

func (f *NetworkPolicyFingerprint) Fingerprint(_ *FingerprintRequest, resp *FingerprintResponse) error {
	if err := f.detector(); err != nil {
		f.logger.Debug("nftables not available, network policies cannot be enforced", "error", err)
		resp.RemoveAttribute(structs.NetworkPolicyNftablesAttr)
		resp.Detected = true
		return nil
	}
	resp.AddAttribute(structs.NetworkPolicyNftablesAttr, "true")
	resp.Detected = true
	return nil
}

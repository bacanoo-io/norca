// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fingerprint

func initPlatformFingerprints(fps map[string]Factory) {
	fps["cgroup"] = NewCGroupFingerprint
	fps["bridge"] = NewBridgeFingerprint
	fps["network_policy"] = NewNetworkPolicyFingerprint
}

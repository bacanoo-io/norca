// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

const (
	// NodeScalingGroup is the group of the kinds of node scaling, which the
	// core registers: NodeGroup (a capacity group of nodes) and NodeProvider
	// (an application that creates the machines of a platform).
	NodeScalingGroup = "norca.io"
	NodeGroupKind    = "NodeGroup"
	NodeProviderKind = "NodeProvider"
)

const (
	// NodeMetaNodeGroup is the meta key of a node with the name of the
	// NodeGroup the node scaling operator created it for.
	NodeMetaNodeGroup = "norca.node_group"
	// NodeMetaCertExpires is the meta key of a node with the expiry of its
	// certificate (RFC 3339), which the bootstrap of the node scaling
	// operator writes; a node that renews its certificate itself (norca-join)
	// has none.
	NodeMetaCertExpires = "norca.cert_expires"
)

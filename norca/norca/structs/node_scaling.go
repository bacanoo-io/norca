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

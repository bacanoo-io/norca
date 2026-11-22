// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

// The registries of Rigger, the package manager (design/rig-integration.md
// 4.5): a cluster scoped object per registry, synced when someone asks for
// it.
const (
	// RiggerObjectGroup is the group of the objects of Rigger.
	RiggerObjectGroup = "rigger.norca.io"

	// RiggerRegistryKind is the kind of a registry of modules and stacks.
	RiggerRegistryKind = "Registry"

	// RiggerRegistryKindRef is its kind reference, as ACL rules name it.
	RiggerRegistryKindRef = RiggerObjectGroup + "/" + RiggerRegistryKind

	// RiggerGlobalRegistry is the name of the registry the servers create:
	// the global registry, the only one of the type global.
	RiggerGlobalRegistry = "global"

	// RiggerOperationKind is the kind of an operation on a stack (an apply
	// or a destroy) that the servers run (design/rig-integration.md 4.6b).
	RiggerOperationKind = "Operation"

	// RiggerOperationKindRef is its kind reference, as ACL rules name it.
	RiggerOperationKindRef = RiggerObjectGroup + "/" + RiggerOperationKind
)

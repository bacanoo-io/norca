// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package volumewatcher

import (
	"github.com/bacanoo-io/norca/nomad/nomad/structs"
)

// CSIVolumeRPC is a minimal interface of the Server, intended as an aid
// for testing logic surrounding server-to-server or server-to-client
// RPC calls and to avoid circular references between the nomad
// package and the volumewatcher
type CSIVolumeRPC interface {
	Unpublish(args *structs.CSIVolumeUnpublishRequest, reply *structs.CSIVolumeUnpublishResponse) error
}

// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package asset

import _ "embed"

//go:embed example.norca.hcl
var JobExample []byte

//go:embed example-short.norca.hcl
var JobExampleShort []byte

//go:embed connect.norca.hcl
var JobConnect []byte

//go:embed connect-short.norca.hcl
var JobConnectShort []byte

//go:embed pool.norca.hcl
var NodePoolSpec []byte

//go:embed pool.nomad.json
var NodePoolSpecJSON []byte

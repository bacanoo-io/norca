// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"github.com/bacanoo-io/norca/nomad/api"
)

// testVariable returns a test variable spec
func testVariable() *api.Variable {
	return &api.Variable{
		Namespace: "default",
		Path:      "test/var",
		Items: map[string]string{
			"keyA": "valueA",
			"keyB": "valueB",
		},
	}
}

// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package serviceregistration

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/stretchr/testify/require"
)

func TestWorkloadServices_RegistrationProvider(t *testing.T) {
	testCases := []struct {
		inputWorkloadServices *WorkloadServices
		expectedOutput        string
		name                  string
	}{
		{
			inputWorkloadServices: &WorkloadServices{
				Services: nil,
			},
			expectedOutput: "",
			name:           "nil panic check",
		},
		{
			inputWorkloadServices: &WorkloadServices{
				Services: []*structs.Service{
					{Provider: structs.ServiceProviderNomad},
				},
			},
			expectedOutput: "nomad",
			name:           "nomad provider",
		},
		{
			inputWorkloadServices: &WorkloadServices{
				Services: []*structs.Service{
					{Provider: structs.ServiceProviderConsul},
				},
			},
			expectedOutput: "consul",
			name:           "consul provider",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actualOutput := tc.inputWorkloadServices.RegistrationProvider()
			require.Equal(t, tc.expectedOutput, actualOutput)
		})
	}
}

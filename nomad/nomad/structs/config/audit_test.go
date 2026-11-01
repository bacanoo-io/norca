// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/nomad/ci"
	"github.com/bacanoo-io/norca/nomad/helper/pointer"
	"github.com/stretchr/testify/require"
)

func TestAuditConfig_Merge(t *testing.T) {
	ci.Parallel(t)

	c1 := &AuditConfig{
		Enabled: pointer.Of(true),
		Sinks: []*AuditSink{
			{
				DeliveryGuarantee: "enforced",
				Name:              "file",
				Type:              "file",
				Format:            "json",
				Path:              "/opt/nomad/audit.log",
				RotateDuration:    24 * time.Hour,
				RotateDurationHCL: "24h",
				RotateBytes:       100,
				RotateMaxFiles:    10,
			},
		},
		Filters: []*AuditFilter{
			{
				Name:       "one",
				Type:       "HTTPEvent",
				Endpoints:  []string{"/v1/metrics"},
				Stages:     []string{"*"},
				Operations: []string{"*"},
			},
		},
	}

	c2 := &AuditConfig{
		Sinks: []*AuditSink{
			{
				DeliveryGuarantee: "best-effort",
				Name:              "file",
				Type:              "file",
				Format:            "json",
				Path:              "/opt/nomad/audit.log",
				RotateDuration:    48 * time.Hour,
				RotateDurationHCL: "48h",
				RotateBytes:       20,
				RotateMaxFiles:    2,
			},
		},
		Filters: []*AuditFilter{
			{
				Name:       "one",
				Type:       "HTTPEvent",
				Endpoints:  []string{"/v1/metrics"},
				Stages:     []string{"OperationReceived"},
				Operations: []string{"GET"},
			},
			{
				Name:       "two",
				Type:       "HTTPEvent",
				Endpoints:  []string{"*"},
				Stages:     []string{"OperationReceived"},
				Operations: []string{"OPTIONS"},
			},
		},
	}

	e := &AuditConfig{
		Enabled: pointer.Of(true),
		Sinks: []*AuditSink{
			{
				DeliveryGuarantee: "best-effort",
				Name:              "file",
				Type:              "file",
				Format:            "json",
				Path:              "/opt/nomad/audit.log",
				RotateDuration:    48 * time.Hour,
				RotateDurationHCL: "48h",
				RotateBytes:       20,
				RotateMaxFiles:    2,
			},
		},
		Filters: []*AuditFilter{
			{
				Name:       "one",
				Type:       "HTTPEvent",
				Endpoints:  []string{"/v1/metrics"},
				Stages:     []string{"OperationReceived"},
				Operations: []string{"GET"},
			},
			{
				Name:       "two",
				Type:       "HTTPEvent",
				Endpoints:  []string{"*"},
				Stages:     []string{"OperationReceived"},
				Operations: []string{"OPTIONS"},
			},
		},
	}

	result := c1.Merge(c2)

	require.Equal(t, e, result)
}

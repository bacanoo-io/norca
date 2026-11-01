// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"bytes"
	"testing"

	"github.com/bacanoo-io/norca/nomad/ci"
	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/require"
)

func TestHttpServerLoggerFilters_Level_Info(t *testing.T) {
	ci.Parallel(t)

	var buf bytes.Buffer
	hclogger := hclog.New(&hclog.LoggerOptions{
		Name:   "testlog",
		Output: &buf,
		Level:  hclog.Info,
	})

	stdlogger := newHTTPServerLogger(hclogger)

	// spurious logging would be filtered out
	stdlogger.Printf("spurious logging: %v", "arg")
	require.Empty(t, buf.String())

	// panics are included
	stdlogger.Printf("panic while processing: %v", "endpoint")
	require.Contains(t, buf.String(), "[ERROR] testlog: panic while processing: endpoint")

}

func TestHttpServerLoggerFilters_Level_Trace(t *testing.T) {
	ci.Parallel(t)

	var buf bytes.Buffer
	hclogger := hclog.New(&hclog.LoggerOptions{
		Name:   "testlog",
		Output: &buf,
		Level:  hclog.Trace,
	})

	stdlogger := newHTTPServerLogger(hclogger)

	// spurious logging will be included as Trace level
	stdlogger.Printf("spurious logging: %v", "arg")
	require.Contains(t, buf.String(), "[TRACE] testlog: spurious logging: arg")

	stdlogger.Printf("panic while processing: %v", "endpoint")
	require.Contains(t, buf.String(), "[ERROR] testlog: panic while processing: endpoint")

}

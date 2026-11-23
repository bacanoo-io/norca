// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/rigger/fetch"
	"github.com/shoenig/test/must"
)

// parseRiggerConfig parses a configuration file with the content hcl.
func parseRiggerConfig(t *testing.T, hcl string) (*Config, error) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "rigger.hcl")
	must.NoError(t, os.WriteFile(path, []byte(hcl), 0o600))

	return ParseConfigFile(path)
}

func TestConfig_Rigger_Parse(t *testing.T) {
	ci.Parallel(t)

	parsed, err := parseRiggerConfig(t, `
server {
  enabled = true
  rigger {
    fetch_tree_max_bytes = "2GiB"
    fetch_file_max_bytes = "64MiB"
    fetch_tree_max_files = 5000
  }
}`)
	must.NoError(t, err)

	c := DefaultConfig().Merge(parsed)
	must.Eq(t, "2GiB", c.Server.Rigger.FetchTreeMaxBytes)
	must.Eq(t, "64MiB", c.Server.Rigger.FetchFileMaxBytes)
	must.Eq(t, 5000, c.Server.Rigger.FetchTreeMaxFiles)

	blocks, err := convertNorcaServerBlocks(c.Server, c.Ports)
	must.NoError(t, err)
	must.Eq(t, fetch.Limits{TreeBytes: 2 << 30, FileBytes: 64 << 20, TreeFiles: 5000}, blocks.riggerFetchLimits)

	// A size without a unit is a number of bytes, with or without quotes.
	parsed, err = parseRiggerConfig(t, `server { rigger { fetch_tree_max_bytes = 4096  fetch_file_max_bytes = "512" } }`)
	must.NoError(t, err)

	limits, err := parsed.Server.Rigger.fetchLimits()
	must.NoError(t, err)
	must.Eq(t, fetch.Limits{TreeBytes: 4096, FileBytes: 512}, limits)

	// Unknown keys are refused.
	_, err = parseRiggerConfig(t, `server { rigger { fetch_max_bytes = "1GiB" } }`)
	must.ErrorContains(t, err, "fetch_max_bytes")
}

// Without a rigger block, and for every limit that is not set or is zero,
// the block sets no limit: the server takes its default.
func TestConfig_Rigger_Defaults(t *testing.T) {
	ci.Parallel(t)

	blocks, err := convertNorcaServerBlocks(DefaultConfig().Server, nil)
	must.NoError(t, err)
	must.Eq(t, fetch.Limits{}, blocks.riggerFetchLimits)

	tests := []struct {
		name     string
		block    *RiggerConfig
		expected fetch.Limits
	}{
		{name: "no block", block: nil},
		{name: "an empty block", block: &RiggerConfig{}},
		{name: "zero", block: &RiggerConfig{FetchTreeMaxBytes: "0", FetchFileMaxBytes: "0", FetchTreeMaxFiles: 0}},
		{
			name:     "one limit set",
			block:    &RiggerConfig{FetchFileMaxBytes: "8MiB"},
			expected: fetch.Limits{FileBytes: 8 << 20},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limits, err := tt.block.fetchLimits()
			must.NoError(t, err)
			must.Eq(t, tt.expected, limits)
		})
	}
}

// A later file sets the limits it names and keeps the others.
func TestConfig_Rigger_Merge(t *testing.T) {
	ci.Parallel(t)

	first, err := parseRiggerConfig(t, `server { rigger { fetch_tree_max_bytes = "2GiB"  fetch_tree_max_files = 5000 } }`)
	must.NoError(t, err)

	second, err := parseRiggerConfig(t, `server { rigger { fetch_file_max_bytes = "8MiB"  fetch_tree_max_files = 7 } }`)
	must.NoError(t, err)

	// A file without the block keeps it.
	none, err := parseRiggerConfig(t, `server { enabled = true }`)
	must.NoError(t, err)

	c := DefaultConfig().Merge(first).Merge(second).Merge(none)
	must.Eq(t, &RiggerConfig{FetchTreeMaxBytes: "2GiB", FetchFileMaxBytes: "8MiB", FetchTreeMaxFiles: 7}, c.Server.Rigger)

	blocks, err := convertNorcaServerBlocks(c.Server, c.Ports)
	must.NoError(t, err)
	must.Eq(t, fetch.Limits{TreeBytes: 2 << 30, FileBytes: 8 << 20, TreeFiles: 7}, blocks.riggerFetchLimits)

	// The merge and the copy do not share the blocks they are made of.
	must.Eq(t, "", first.Server.Rigger.FetchFileMaxBytes)
	must.Eq(t, 5000, first.Server.Rigger.FetchTreeMaxFiles)

	copied := c.Server.Copy()
	must.Eq(t, c.Server.Rigger, copied.Rigger)

	copied.Rigger.FetchTreeMaxFiles = 1
	must.Eq(t, 7, c.Server.Rigger.FetchTreeMaxFiles)

	var unset *RiggerConfig
	must.Nil(t, unset.Copy())
	must.Nil(t, unset.Merge(nil))
	must.Eq(t, &RiggerConfig{FetchTreeMaxFiles: 3}, unset.Merge(&RiggerConfig{FetchTreeMaxFiles: 3}))
	must.Eq(t, &RiggerConfig{FetchTreeMaxFiles: 3}, (&RiggerConfig{FetchTreeMaxFiles: 3}).Merge(nil))
}

// A limit that is not a size, or is negative, is an error of the
// configuration: of the block, of config validate and of the start of the
// server.
func TestConfig_Rigger_Invalid(t *testing.T) {
	ci.Parallel(t)

	tests := []struct {
		name     string
		block    *RiggerConfig
		expected string
	}{
		{
			name:     "the bytes of a tree are not a size",
			block:    &RiggerConfig{FetchTreeMaxBytes: "lots"},
			expected: "rigger.fetch_tree_max_bytes must be a size that is not negative, not lots",
		},
		{
			name:     "the bytes of a tree are negative",
			block:    &RiggerConfig{FetchTreeMaxBytes: "-1GiB"},
			expected: "rigger.fetch_tree_max_bytes must be a size that is not negative, not -1GiB",
		},
		{
			name:     "the bytes of a tree are past what a server counts",
			block:    &RiggerConfig{FetchTreeMaxBytes: "9EiB"},
			expected: "rigger.fetch_tree_max_bytes must be a size that is not negative, not 9EiB",
		},
		{
			name:     "the bytes of a file are not a size",
			block:    &RiggerConfig{FetchFileMaxBytes: "1 gigabyte of files"},
			expected: "rigger.fetch_file_max_bytes must be a size that is not negative, not 1 gigabyte of files",
		},
		{
			name:     "the bytes of a file are negative",
			block:    &RiggerConfig{FetchFileMaxBytes: "-1"},
			expected: "rigger.fetch_file_max_bytes must be a size that is not negative, not -1",
		},
		{
			name:     "the files of a tree are negative",
			block:    &RiggerConfig{FetchTreeMaxFiles: -1},
			expected: "rigger.fetch_tree_max_files must not be negative, not -1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.block.fetchLimits()
			must.ErrorContains(t, err, tt.expected)

			_, err = convertNorcaServerBlocks(&ServerConfig{Rigger: tt.block}, nil)
			must.ErrorContains(t, err, "invalid server rigger configuration: "+tt.expected)
		})
	}

	// A negative number of a file is refused as config validate and the
	// start of the agent check it.
	for _, hcl := range []string{
		`server { enabled = true  rigger { fetch_tree_max_bytes = -1 } }`,
		`server { enabled = true  rigger { fetch_file_max_bytes = -1 } }`,
		`server { enabled = true  rigger { fetch_tree_max_files = -1 } }`,
	} {
		parsed, err := parseRiggerConfig(t, hcl)
		must.NoError(t, err, must.Sprint(hcl))
		must.ErrorContains(t, validateNorcaBlocks(DefaultConfig().Merge(parsed)), "invalid server rigger configuration", must.Sprint(hcl))
	}
}

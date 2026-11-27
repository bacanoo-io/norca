// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"math"
	"slices"

	"github.com/bacanoo-io/norca/rigger/fetch"
	"github.com/dustin/go-humanize"
)

// RiggerConfig is the server's rigger block: the limits of what Rigger
// fetches on the servers. A server fetches the git repositories its users
// name (registries, and the modules and stacks of an install), and writes
// the tree of the commit it reads into <data_dir>/server/rigger/cache. The limits
// bound one fetched commit; a commit past one of them is not written, and
// the sync or the install that asked for it fails with the limit in its
// error. A limit that is not set, or is 0, is the default of a server (a
// tree of 1GiB, a file of 128MiB, 100000 files); a negative one is an error
// of the configuration. The servers read the block when they start.
//
//	rigger {
//	  fetch_tree_max_bytes = "1GiB"
//	  fetch_file_max_bytes = "128MiB"
//	  fetch_tree_max_files = 100000
//	}
type RiggerConfig struct {
	// FetchTreeMaxBytes bounds the files of the tree of one fetched commit
	// together.
	FetchTreeMaxBytes string `hcl:"fetch_tree_max_bytes"`

	// FetchFileMaxBytes bounds one file of the tree of a fetched commit.
	FetchFileMaxBytes string `hcl:"fetch_file_max_bytes"`

	// FetchTreeMaxFiles bounds the number of files of the tree of one
	// fetched commit.
	FetchTreeMaxFiles int `hcl:"fetch_tree_max_files"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

func (r *RiggerConfig) Copy() *RiggerConfig {
	if r == nil {
		return nil
	}
	nr := *r
	nr.ExtraKeysHCL = slices.Clone(r.ExtraKeysHCL)
	return &nr
}

// Merge merges b into a copy of a.
func (a *RiggerConfig) Merge(b *RiggerConfig) *RiggerConfig {
	if a == nil {
		return b.Copy()
	}
	result := a.Copy()
	if b == nil {
		return result
	}
	if b.FetchTreeMaxBytes != "" {
		result.FetchTreeMaxBytes = b.FetchTreeMaxBytes
	}
	if b.FetchFileMaxBytes != "" {
		result.FetchFileMaxBytes = b.FetchFileMaxBytes
	}
	if b.FetchTreeMaxFiles != 0 {
		result.FetchTreeMaxFiles = b.FetchTreeMaxFiles
	}
	return result
}

// fetchLimits converts the block into the limits of the fetcher of the
// server and validates it. A limit that is not set, or is 0, stays 0: the
// server takes its default for it (norca.Config.RiggerFetchLimits).
func (r *RiggerConfig) fetchLimits() (fetch.Limits, error) {
	var limits fetch.Limits
	if r == nil {
		return limits, nil
	}
	sizes := []struct {
		name  string
		value string
		field *int64
	}{
		{"fetch_tree_max_bytes", r.FetchTreeMaxBytes, &limits.TreeBytes},
		{"fetch_file_max_bytes", r.FetchFileMaxBytes, &limits.FileBytes},
	}
	for _, s := range sizes {
		if s.value == "" {
			continue
		}
		// A size has no sign: a negative one does not parse.
		v, err := humanize.ParseBytes(s.value)
		if err != nil || v > math.MaxInt64 {
			return fetch.Limits{}, fmt.Errorf("rigger.%s must be a size that is not negative, not %s", s.name, s.value)
		}
		*s.field = int64(v)
	}
	if r.FetchTreeMaxFiles < 0 {
		return fetch.Limits{}, fmt.Errorf("rigger.fetch_tree_max_files must not be negative, not %d", r.FetchTreeMaxFiles)
	}
	limits.TreeFiles = r.FetchTreeMaxFiles
	return limits, nil
}

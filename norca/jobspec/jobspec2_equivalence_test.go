// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// The tests that jobspec2 parses the files of jobspec (v1) to the same job.
// They were in jobspec2/parse_test.go; they moved here when jobspec2 became
// a module of its own, which does not import the norca program module.

package jobspec_test

import (
	"os"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/jobspec"
	"github.com/bacanoo-io/norca/norca/jobspec2"
	"github.com/stretchr/testify/require"
)

func TestEquivalentToHCL1(t *testing.T) {
	ci.Parallel(t)

	hclSpecDir := "test-fixtures/"
	fis, err := os.ReadDir(hclSpecDir)
	require.NoError(t, err)

	for _, fi := range fis {
		name := fi.Name()

		t.Run(name, func(t *testing.T) {
			f, err := os.Open(hclSpecDir + name)
			require.NoError(t, err)
			defer f.Close()

			job1, err := jobspec.Parse(f)
			if err != nil {
				t.Skip("file is not parsable in v1")
			}

			f.Seek(0, 0)

			job2, err := jobspec2.Parse(name, f)
			require.NoError(t, err)

			require.Equal(t, job1, job2)
		})
	}
}

func TestEquivalentToHCL1_ComplexConfig(t *testing.T) {
	ci.Parallel(t)

	name := "../jobspec2/test-fixtures/config-compatibility.hcl"
	f, err := os.Open(name)
	require.NoError(t, err)
	defer f.Close()

	job1, err := jobspec.Parse(f)
	require.NoError(t, err)

	f.Seek(0, 0)

	job2, err := jobspec2.Parse(name, f)
	require.NoError(t, err)

	require.Equal(t, job1, job2)
}

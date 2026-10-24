// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"strings"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/stretchr/testify/assert"
)

func TestEvalList_ArgsWithoutPageToken(t *testing.T) {
	ci.Parallel(t)

	cases := []struct {
		cli      string
		expected string
	}{
		{
			cli:      "norca eval list -page-token=abcdef",
			expected: "norca eval list",
		},
		{
			cli:      "norca eval list -page-token abcdef",
			expected: "norca eval list",
		},
		{
			cli:      "norca eval list -per-page 3 -page-token abcdef",
			expected: "norca eval list -per-page 3",
		},
		{
			cli:      "norca eval list -page-token abcdef -per-page 3",
			expected: "norca eval list -per-page 3",
		},
		{
			cli:      "norca eval list -per-page=3 -page-token abcdef",
			expected: "norca eval list -per-page=3",
		},
		{
			cli:      "norca eval list -verbose -page-token abcdef",
			expected: "norca eval list -verbose",
		},
		{
			cli:      "norca eval list -page-token abcdef -verbose",
			expected: "norca eval list -verbose",
		},
		{
			cli:      "norca eval list -verbose -page-token abcdef -per-page 3",
			expected: "norca eval list -verbose -per-page 3",
		},
		{
			cli:      "norca eval list -page-token abcdef -verbose -per-page 3",
			expected: "norca eval list -verbose -per-page 3",
		},
	}

	for _, tc := range cases {
		args := strings.Split(tc.cli, " ")
		assert.Equal(t, tc.expected, argsWithoutPageToken(args),
			"for input: %s", tc.cli)
	}

}

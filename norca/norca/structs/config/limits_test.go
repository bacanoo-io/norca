// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/stretchr/testify/require"
)

// TestLimits_Defaults asserts the default limits are valid.
func TestLimits_Defaults(t *testing.T) {
	ci.Parallel(t)

	l := DefaultLimits()
	d, err := time.ParseDuration(l.HTTPSHandshakeTimeout)
	require.NoError(t, err)
	require.True(t, d > 0)

	d, err = time.ParseDuration(l.RPCHandshakeTimeout)
	require.NoError(t, err)
	require.True(t, d > 0)
}

// TestLimits_Copy asserts Limits structs are deep copied.
func TestLimits_Copy(t *testing.T) {
	ci.Parallel(t)

	o := DefaultLimits()
	c := o.Copy()

	// Assert changes to copy are not propagated to the original
	c.HTTPSHandshakeTimeout = "1s"
	c.HTTPMaxConnsPerClient = pointer.Of(50)
	c.RPCHandshakeTimeout = "1s"
	c.RPCMaxConnsPerClient = pointer.Of(50)

	require.NotEqual(t, c.HTTPSHandshakeTimeout, o.HTTPSHandshakeTimeout)

	// Pointers should be different
	require.True(t, c.HTTPMaxConnsPerClient != o.HTTPMaxConnsPerClient)

	require.NotEqual(t, c.HTTPMaxConnsPerClient, o.HTTPMaxConnsPerClient)
	require.NotEqual(t, c.RPCHandshakeTimeout, o.RPCHandshakeTimeout)

	// Pointers should be different
	require.True(t, c.RPCMaxConnsPerClient != o.RPCMaxConnsPerClient)

	require.NotEqual(t, c.RPCMaxConnsPerClient, o.RPCMaxConnsPerClient)
}

// TestLimits_Merge asserts non-zero fields from the method argument take
// precedence over the existing limits.
func TestLimits_Merge(t *testing.T) {
	ci.Parallel(t)

	l := Limits{}
	o := DefaultLimits()
	m := l.Merge(o)

	// Operands should not change
	require.Equal(t, Limits{}, l)
	require.Equal(t, DefaultLimits(), o)

	// m == o
	require.Equal(t, m, DefaultLimits())

	o.HTTPSHandshakeTimeout = "10s"
	m2 := m.Merge(o)

	// Operands should not change
	require.Equal(t, m, DefaultLimits())

	// Use short struct initialization style so it fails to compile if
	// fields are added
	expected := Limits{"10s", pointer.Of(100), "5s", pointer.Of(100), nil, nil, nil, nil, pointer.Of(1 << 20)}
	require.Equal(t, expected, m2)

	// The token rate limits (Norca) merge like the other pointers.
	o.TokenReadRequestsPerSecond = pointer.Of(50)
	o.TokenWriteBurst = pointer.Of(5)
	m4 := m2.Merge(o)
	require.Equal(t, 50, *m4.TokenReadRequestsPerSecond)
	require.Equal(t, 5, *m4.TokenWriteBurst)
	require.Nil(t, m4.TokenWriteRequestsPerSecond)
	c := m4.Copy()
	*c.TokenReadRequestsPerSecond = 1
	require.Equal(t, 50, *m4.TokenReadRequestsPerSecond)

	// Mergin in 0 values should not change anything
	m3 := m2.Merge(Limits{})
	require.Equal(t, m2, m3)
}

// TestLimits_ExecMaxFrameBytes: exec_max_frame_bytes (Norca) has a default of
// 1 MiB, merges and copies like the other pointers, 0 turns the limit off,
// and a value that is negative or too small for the command line client is
// refused.
func TestLimits_ExecMaxFrameBytes(t *testing.T) {
	ci.Parallel(t)

	d := DefaultLimits()
	require.Equal(t, 1<<20, *d.ExecMaxFrameBytes)

	// a set value wins over the default, also 0; an unset one does not
	m := d.Merge(Limits{ExecMaxFrameBytes: pointer.Of(8192)})
	require.Equal(t, 8192, *m.ExecMaxFrameBytes)
	require.Equal(t, 1<<20, *d.ExecMaxFrameBytes)
	require.Equal(t, 0, *d.Merge(Limits{ExecMaxFrameBytes: pointer.Of(0)}).ExecMaxFrameBytes)
	require.Equal(t, 8192, *m.Merge(Limits{}).ExecMaxFrameBytes)

	// the copy has its own value
	c := m.Copy()
	*c.ExecMaxFrameBytes = 1
	require.Equal(t, 8192, *m.ExecMaxFrameBytes)

	cases := []struct {
		name    string
		value   *int
		want    int64
		wantErr string
	}{
		{name: "unset is the default", value: nil, want: 1 << 20},
		{name: "0 is no limit", value: pointer.Of(0), want: 0},
		{name: "the smallest limit", value: pointer.Of(4096), want: 4096},
		{name: "a large limit", value: pointer.Of(64 << 20), want: 64 << 20},
		{name: "negative", value: pointer.Of(-1), wantErr: "exec_max_frame_bytes must be >= 0"},
		{name: "under the smallest limit", value: pointer.Of(4095),
			wantErr: "exec_max_frame_bytes must be 0 (no limit) or at least 4096"},
		{name: "1", value: pointer.Of(1),
			wantErr: "exec_max_frame_bytes must be 0 (no limit) or at least 4096"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := Limits{ExecMaxFrameBytes: tc.value}
			got, err := l.ExecMaxFrame()
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

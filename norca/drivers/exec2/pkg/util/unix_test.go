// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package util

import (
	"testing"

	"github.com/shoenig/test/must"
)

func Test_IsLinuxOS(t *testing.T) {
	must.True(t, IsLinuxOS())
}

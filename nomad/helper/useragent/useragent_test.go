// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package useragent

import (
	"testing"
)

func TestUserAgent(t *testing.T) {
	norcaVersionFunc = func() string { return "0.9.1" }
	rt = "go5.0"
	versionFunc = func() string { return "1.2.3" }

	act := String()

	exp := "Norca/0.9.1 (Nomad API 1.2.3; go5.0)"
	if exp != act {
		t.Errorf("expected %q to be %q", act, exp)
	}
}

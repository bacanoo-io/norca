// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build windows

package docker

import (
	"testing"

	"github.com/bacanoo-io/norca/nomad/client/allocdir"
	"github.com/bacanoo-io/norca/nomad/testutil"
)

func newTaskConfig(variant string, command []string) TaskConfig {
	// busyboxImageID is an id of an image containing nanoserver windows and
	// a busybox exe.
	busyboxImageID := testutil.TestBusyboxImage()

	return TaskConfig{
		Image:            busyboxImageID,
		ImagePullTimeout: "5m",
		Command:          command[0],
		Args:             command[1:],
	}
}

// No-op on windows because we don't load images.
func copyImage(t *testing.T, taskDir *allocdir.TaskDir, image string) {
}

// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/client/allocrunner/interfaces"
	"github.com/bacanoo-io/norca/norca/client/pluginmanager/csimanager"
	cstructs "github.com/bacanoo-io/norca/norca/client/structs"
	"github.com/bacanoo-io/norca/norca/client/taskenv"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/plugins/drivers"
	dtu "github.com/bacanoo-io/norca/norca/plugins/drivers/testutils"
	"github.com/stretchr/testify/require"
)

func TestVolumeHook_PartitionMountsByVolume_Works(t *testing.T) {
	ci.Parallel(t)

	mounts := []*structs.VolumeMount{
		{
			Volume:      "foo",
			Destination: "/tmp",
			ReadOnly:    false,
		},
		{
			Volume:      "foo",
			Destination: "/bar",
			ReadOnly:    false,
		},
		{
			Volume:      "baz",
			Destination: "/baz",
			ReadOnly:    false,
		},
	}

	expected := map[string][]*structs.VolumeMount{
		"foo": {
			{
				Volume:      "foo",
				Destination: "/tmp",
				ReadOnly:    false,
			},
			{
				Volume:      "foo",
				Destination: "/bar",
				ReadOnly:    false,
			},
		},
		"baz": {
			{
				Volume:      "baz",
				Destination: "/baz",
				ReadOnly:    false,
			},
		},
	}

	// Test with a real collection

	partitioned := partitionMountsByVolume(mounts)
	require.Equal(t, expected, partitioned)

	// Test with nil/emptylist

	partitioned = partitionMountsByVolume(nil)
	require.Equal(t, map[string][]*structs.VolumeMount{}, partitioned)
}

func TestVolumeHook_prepareCSIVolumes(t *testing.T) {
	ci.Parallel(t)

	req := &interfaces.TaskPrestartRequest{
		Task: &structs.Task{
			Name:   "test",
			Driver: "mock",
			VolumeMounts: []*structs.VolumeMount{
				{
					Volume:      "foo",
					Destination: "/bar",
				},
			},
		},
	}

	volumes := map[string]*structs.VolumeRequest{
		"foo": {
			Type:   "csi",
			Source: "my-test-volume",
		},
	}

	cases := []struct {
		Name          string
		Driver        drivers.DriverPlugin
		Expected      []*drivers.MountConfig
		ExpectedError string
	}{
		{
			Name: "supported driver",
			Driver: &dtu.MockDriver{
				CapabilitiesF: func() (*drivers.Capabilities, error) {
					return &drivers.Capabilities{
						MountConfigs: drivers.MountConfigSupportAll,
					}, nil
				},
			},
			Expected: []*drivers.MountConfig{
				{
					HostPath: "/mnt/my-test-volume",
					TaskPath: "/bar",
				},
			},
		},
		{
			Name: "unsupported driver",
			Driver: &dtu.MockDriver{
				CapabilitiesF: func() (*drivers.Capabilities, error) {
					return &drivers.Capabilities{
						MountConfigs: drivers.MountConfigSupportNone,
					}, nil
				},
			},
			ExpectedError: "task driver \"mock\" for \"test\" does not support CSI",
		},
	}

	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {

			tr := &TaskRunner{
				task:               req.Task,
				driver:             tc.Driver,
				allocHookResources: cstructs.NewAllocHookResources(),
			}
			tr.allocHookResources.SetCSIMounts(map[string]*csimanager.MountInfo{
				"foo": {
					Source: "/mnt/my-test-volume",
				},
			})

			hook := &volumeHook{
				logger: testlog.HCLogger(t),
				alloc:  structs.MockAlloc(),
				runner: tr,
			}
			mounts, err := hook.prepareCSIVolumes(req, volumes)

			if tc.ExpectedError != "" {
				require.EqualError(t, err, tc.ExpectedError)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.Expected, mounts)
		})
	}
}

func TestVolumeHook_Interpolation(t *testing.T) {
	ci.Parallel(t)

	alloc := mock.Alloc()
	task := alloc.Job.TaskGroups[0].Tasks[0]
	taskEnv := taskenv.NewBuilder(mock.Node(), alloc, task, "global").SetHookEnv("volume",
		map[string]string{
			"PROPAGATION_MODE": "private",
			"VOLUME_ID":        "my-other-volume",
		},
	).Build()

	mounts := []*structs.VolumeMount{
		{
			Volume:          "foo",
			Destination:     "/tmp",
			ReadOnly:        false,
			PropagationMode: "bidirectional",
		},
		{
			Volume:          "foo",
			Destination:     "/bar-${NORCA_JOB_NAME}",
			ReadOnly:        false,
			PropagationMode: "bidirectional",
		},
		{
			Volume:          "${VOLUME_ID}",
			Destination:     "/baz",
			ReadOnly:        false,
			PropagationMode: "bidirectional",
		},
		{
			Volume:          "foo",
			Destination:     "/quux",
			ReadOnly:        false,
			PropagationMode: "${PROPAGATION_MODE}",
		},
	}

	expected := []*structs.VolumeMount{
		{
			Volume:          "foo",
			Destination:     "/tmp",
			ReadOnly:        false,
			PropagationMode: "bidirectional",
		},
		{
			Volume:          "foo",
			Destination:     "/bar-my-job",
			ReadOnly:        false,
			PropagationMode: "bidirectional",
		},
		{
			Volume:          "my-other-volume",
			Destination:     "/baz",
			ReadOnly:        false,
			PropagationMode: "bidirectional",
		},
		{
			Volume:          "foo",
			Destination:     "/quux",
			ReadOnly:        false,
			PropagationMode: "private",
		},
	}

	interpolateVolumeMounts(mounts, taskEnv)
	require.Equal(t, expected, mounts)
}

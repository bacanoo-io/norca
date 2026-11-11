// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package csimanager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/client/dynamicplugins"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/plugins/csi"
	csifake "github.com/bacanoo-io/norca/norca/plugins/csi/fake"
	"github.com/shoenig/test/must"
	"google.golang.org/grpc"
)

// TestVolumeManager_collectStats checks the NodeGetVolumeStats requests of
// the published volumes (external ID, target and staging path) and the
// conversion of the answers (clean room: CSI spec v1.7.0
// NodeGetVolumeStats).
func TestVolumeManager_collectStats(t *testing.T) {
	ci.Parallel(t)

	root := t.TempDir()
	fake := &csifake.Client{}
	vm := newVolumeManager(testlog.HCLogger(t), func(*structs.NodeEvent) {}, fake, root, "/csi", true, "i-1")

	usage := &UsageOptions{
		AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem,
		AccessMode:     structs.CSIVolumeAccessModeSingleNodeWriter,
	}
	vm.usageTracker.Claim("alloc-1", "vol-1", usage)
	vm.usageTracker.setRef("vol-1", usage, volumeRef{namespace: "prod", remoteID: "ext-1"})

	fake.NextNodeGetVolumeStatsResponse = &csi.NodeGetVolumeStatsResponse{
		Bytes:     &csi.VolumeUsage{Used: 10, Available: 90, Total: 100},
		Inodes:    &csi.VolumeUsage{Used: 3, Available: 7, Total: 10},
		Condition: &csi.VolumeCondition{Abnormal: true, Message: "disk errors"},
	}
	now := time.Unix(1000, 0)
	out := vm.collectStats(context.Background(), "plug", now)
	must.Len(t, 1, out)

	req := fake.LastNodeGetVolumeStatsRequest
	must.Eq(t, "ext-1", req.ExternalVolumeID)
	must.Eq(t, vm.targetForVolume("/csi", "vol-1", "alloc-1", usage), req.VolumePath)
	must.Eq(t, vm.stagingDirForVolume("/csi", "vol-1", usage), req.StagingPath)

	must.Eq(t, &structs.CSIVolumeNodeUsage{
		Namespace: "prod", VolumeID: "vol-1", PluginID: "plug",
		BytesKnown: true, BytesUsed: 10, BytesAvailable: 90, BytesTotal: 100,
		InodesKnown: true, InodesUsed: 3, InodesFree: 7, InodesTotal: 10,
		ConditionKnown: true, Abnormal: true, Message: "disk errors",
		At: now,
	}, out[0])

	// Only bytes, no condition: the other values are unknown.
	fake.NextNodeGetVolumeStatsResponse = &csi.NodeGetVolumeStatsResponse{
		Bytes: &csi.VolumeUsage{Used: 1, Available: 2, Total: 3},
	}
	out = vm.collectStats(context.Background(), "plug", now)
	must.Len(t, 1, out)
	must.True(t, out[0].BytesKnown)
	must.False(t, out[0].InodesKnown)
	must.False(t, out[0].ConditionKnown)

	// An error is reported with the volume; a volume the plugin no longer
	// has published is left out.
	fake.NextNodeGetVolumeStatsResponse = nil
	fake.NextNodeGetVolumeStatsErr = errors.New("node plugin returned an error: boom")
	out = vm.collectStats(context.Background(), "plug", now)
	must.Len(t, 1, out)
	must.StrContains(t, out[0].Error, "boom")

	fake.NextNodeGetVolumeStatsErr = fmt.Errorf("%w: gone", structs.ErrCSIClientRPCIgnorable)
	must.SliceEmpty(t, vm.collectStats(context.Background(), "plug", now))

	// Freed: nothing to ask.
	must.True(t, vm.usageTracker.Free("alloc-1", "vol-1", usage))
	fake.NextNodeGetVolumeStatsErr = nil
	calls := fake.NodeGetVolumeStatsCallCount
	must.SliceEmpty(t, vm.collectStats(context.Background(), "plug", now))
	must.Eq(t, calls, fake.NodeGetVolumeStatsCallCount)
	must.MapEmpty(t, vm.usageTracker.refs)
}

// TestVolumeManager_collectStats_OneEntryPerVolume checks that a volume
// published under two usages is reported once, the abnormal answer first.
func TestVolumeManager_collectStats_OneEntryPerVolume(t *testing.T) {
	ci.Parallel(t)

	plugin := &abnormalOnReadOnly{}
	vm := newVolumeManager(testlog.HCLogger(t), func(*structs.NodeEvent) {}, plugin, t.TempDir(), "/csi", false, "i-1")
	rw := &UsageOptions{AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem, AccessMode: structs.CSIVolumeAccessModeMultiNodeMultiWriter}
	ro := &UsageOptions{ReadOnly: true, AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem, AccessMode: structs.CSIVolumeAccessModeMultiNodeMultiWriter}
	for _, u := range []*UsageOptions{rw, ro} {
		vm.usageTracker.Claim("alloc-"+u.ToFS(), "vol-1", u)
		vm.usageTracker.setRef("vol-1", u, volumeRef{namespace: "default", remoteID: "ext-1"})
	}

	out := vm.collectStats(context.Background(), "plug", time.Now())
	must.Len(t, 1, out)
	must.True(t, out[0].Abnormal)
	must.Eq(t, "", plugin.lastStaging) // no staging without the capability
}

// abnormalOnReadOnly reports the volume abnormal at its read-only target.
type abnormalOnReadOnly struct {
	csifake.Client
	lastStaging string
}

func (p *abnormalOnReadOnly) NodeGetVolumeStats(_ context.Context, req *csi.NodeGetVolumeStatsRequest, _ ...grpc.CallOption) (*csi.NodeGetVolumeStatsResponse, error) {
	p.lastStaging = req.StagingPath
	abnormal := strings.Contains(req.VolumePath, "ro-")
	return &csi.NodeGetVolumeStatsResponse{Condition: &csi.VolumeCondition{Abnormal: abnormal}}, nil
}

// TestInstanceManager_statsInterval checks the interval from the plugin
// registration: the default without it, 0 when turned off.
func TestInstanceManager_statsInterval(t *testing.T) {
	ci.Parallel(t)

	for opt, want := range map[string]time.Duration{
		"":    structs.DefaultCSIVolumeStatsInterval,
		"bad": structs.DefaultCSIVolumeStatsInterval,
		"-1s": structs.DefaultCSIVolumeStatsInterval,
		"0s":  0,
		"30s": 30 * time.Second,
	} {
		i := &instanceManager{info: &dynamicplugins.PluginInfo{Options: map[string]string{}}}
		if opt != "" {
			i.info.Options["StatsInterval"] = opt
		}
		must.Eq(t, want, i.statsInterval(), must.Sprintf("option %q", opt))
	}
}

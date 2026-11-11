// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import "time"

// CSIVolumeNodeUsage is the usage and the condition of a CSI volume on one
// client node, as its node plugin reports them to NodeGetVolumeStats (Norca,
// clean room, design csi-completion-plan.md, phase 2). Clients send them with
// their usage report on the system bus (TaskGroupUsageReport.Volumes); the
// servers keep the latest one per volume and node in memory, never in Raft.
type CSIVolumeNodeUsage struct {
	Namespace string
	VolumeID  string
	PluginID  string

	// NodeID is the node of the report; the server sets it.
	NodeID string `json:",omitempty"`

	// The bytes and inodes the plugin reported; a unit it did not report
	// has its Known flag false and zero values.
	BytesKnown     bool
	BytesUsed      int64
	BytesAvailable int64
	BytesTotal     int64

	InodesKnown bool
	InodesUsed  int64
	InodesFree  int64
	InodesTotal int64

	// ConditionKnown is true when the plugin reports a condition (the
	// VOLUME_CONDITION node capability); Abnormal and Message are its value.
	ConditionKnown bool
	Abnormal       bool
	Message        string `json:",omitempty"`

	// Error is why the plugin could not be asked, if it could not.
	Error string `json:",omitempty"`

	// At is when the client asked the plugin.
	At time.Time
}

// Copy returns a copy of the usage.
func (u *CSIVolumeNodeUsage) Copy() *CSIVolumeNodeUsage {
	if u == nil {
		return nil
	}
	c := *u
	return &c
}

const (
	// TopicCSIVolume is the event stream topic of CSI volumes: the changes
	// of their condition (Norca).
	TopicCSIVolume Topic = "CSIVolume"

	// TypeCSIVolumeAbnormal: a node plugin reports a volume abnormal.
	TypeCSIVolumeAbnormal = "VolumeAbnormal"

	// TypeCSIVolumeRecovered: a volume reported abnormal is normal again, or
	// is no longer published on the node.
	TypeCSIVolumeRecovered = "VolumeRecovered"
)

// CSIVolumeConditionEvent is the payload of a CSIVolume event.
type CSIVolumeConditionEvent struct {
	Namespace string
	VolumeID  string
	PluginID  string
	NodeID    string
	Abnormal  bool
	Message   string
	At        time.Time
}

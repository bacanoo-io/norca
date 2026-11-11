// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package structs

import "time"

// CSIVolumeControllerState is the state of a CSI volume as a controller with
// the GET_VOLUME capability reports it to ControllerGetVolume, next to the
// claims of Norca (Norca, clean room, design csi-completion-plan.md, phase 4).
// The leader asks every few minutes and keeps the answer in memory; volume
// status -live asks at once. It is never stored.
type CSIVolumeControllerState struct {
	// CapacityBytes is the size of the volume the controller reports.
	CapacityBytes int64

	// PublishedNodesKnown is true when the controller has
	// LIST_VOLUMES_PUBLISHED_NODES: only then is PublishedExternalNodeIDs
	// complete, and Missing and Extra set.
	PublishedNodesKnown bool

	// PublishedExternalNodeIDs are the nodes (their external IDs, as the
	// plugin names them) the controller has the volume published on.
	PublishedExternalNodeIDs []string `json:",omitempty"`

	// MissingExternalNodeIDs are nodes with a claim of Norca that the
	// controller does not list as published: a drift.
	MissingExternalNodeIDs []string `json:",omitempty"`

	// ExtraExternalNodeIDs are nodes the controller lists without a claim
	// of Norca. The CSI specification allows a controller to list nodes the
	// volume is not published to, so they are not a drift by themselves.
	ExtraExternalNodeIDs []string `json:",omitempty"`

	// ConditionKnown is true when the controller reports a condition
	// (VOLUME_CONDITION); Abnormal and Message are its value.
	ConditionKnown bool
	Abnormal       bool
	Message        string `json:",omitempty"`

	// Error is why the controller could not be asked, if it could not.
	Error string `json:",omitempty"`

	// At is when the controller was asked.
	At time.Time
}

// Copy returns a copy of the state.
func (s *CSIVolumeControllerState) Copy() *CSIVolumeControllerState {
	if s == nil {
		return nil
	}
	out := *s
	out.PublishedExternalNodeIDs = append([]string(nil), s.PublishedExternalNodeIDs...)
	out.MissingExternalNodeIDs = append([]string(nil), s.MissingExternalNodeIDs...)
	out.ExtraExternalNodeIDs = append([]string(nil), s.ExtraExternalNodeIDs...)
	return &out
}

const (
	// TypeCSIVolumeDrift: nodes with a claim of Norca are not published as
	// the controller sees it (CSIVolume topic, Norca).
	TypeCSIVolumeDrift = "VolumeDrift"
)

// CSIVolumeDriftEvent is the payload of a VolumeDrift event.
type CSIVolumeDriftEvent struct {
	Namespace string
	VolumeID  string
	PluginID  string

	// MissingExternalNodeIDs are the claimed nodes the controller does not
	// list as published; ExtraExternalNodeIDs the nodes it lists without a
	// claim, for information.
	MissingExternalNodeIDs []string
	ExtraExternalNodeIDs   []string `json:",omitempty"`

	At time.Time
}

// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"

	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/dustin/go-humanize"
)

// volumeRequestSpec is the spec of a CSIVolumeRequest object. The name of
// the object is the ID of the volume in its namespace.
type volumeRequestSpec struct {
	PluginID        string                      `json:"plugin_id"`
	Mode            string                      `json:"mode"`
	ExternalID      string                      `json:"external_id"`
	DisplayName     string                      `json:"display_name"`
	CapacityMin     string                      `json:"capacity_min"`
	CapacityMax     string                      `json:"capacity_max"`
	Capability      oneOrMany[volumeCapability] `json:"capability"`
	MountOptions    *volumeMountOptions         `json:"mount_options"`
	TopologyRequest *volumeTopologyRequest      `json:"topology_request"`
	Parameters      map[string]string           `json:"parameters"`
	Context         map[string]string           `json:"context"`
	SecretPath      string                      `json:"secret_path"`
	SnapshotID      string                      `json:"snapshot_id"`
	CloneID         string                      `json:"clone_id"`
	DeletionPolicy  string                      `json:"deletion_policy"`
	Adopt           bool                        `json:"adopt"`
}

type volumeCapability struct {
	AccessMode     string `json:"access_mode"`
	AttachmentMode string `json:"attachment_mode"`
}

type volumeMountOptions struct {
	FSType     string   `json:"fs_type"`
	MountFlags []string `json:"mount_flags"`
}

type volumeTopologyRequest struct {
	Required  oneOrMany[volumeTopology] `json:"required"`
	Preferred oneOrMany[volumeTopology] `json:"preferred"`
}

type volumeTopology struct {
	Segments map[string]string `json:"segments"`
}

// oneOrMany is a list that is also written as a single item: HCL gives a
// block that appears once as an object and a repeated one as a list. The
// schema of the kind can't describe that, so the items are decoded strictly
// here: a key they don't have is an error, not dropped.
type oneOrMany[T any] []T

func (o *oneOrMany[T]) UnmarshalJSON(b []byte) error {
	strict := func(out any) error {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		return dec.Decode(out)
	}
	if trimmed := bytes.TrimSpace(b); len(trimmed) == 0 || trimmed[0] == '[' || string(trimmed) == "null" {
		var list []T
		if err := strict(&list); err != nil {
			return err
		}
		*o = list
		return nil
	}
	var one T
	if err := strict(&one); err != nil {
		return err
	}
	*o = []T{one}
	return nil
}

// volumeAccessModes and volumeAttachmentModes are the modes a request may
// ask for.
var (
	volumeAccessModes = []structs.CSIVolumeAccessMode{
		structs.CSIVolumeAccessModeSingleNodeReader,
		structs.CSIVolumeAccessModeSingleNodeWriter,
		structs.CSIVolumeAccessModeSingleNodeSingleWriter,
		structs.CSIVolumeAccessModeSingleNodeMultiWriter,
		structs.CSIVolumeAccessModeMultiNodeReader,
		structs.CSIVolumeAccessModeMultiNodeSingleWriter,
		structs.CSIVolumeAccessModeMultiNodeMultiWriter,
	}
	volumeAttachmentModes = []structs.CSIVolumeAttachmentMode{
		structs.CSIVolumeAttachmentModeFilesystem,
		structs.CSIVolumeAttachmentModeBlockDevice,
	}
)

// mode returns the mode of the request, "create" when it is not set.
func (s *volumeRequestSpec) mode() string {
	if s.Mode == "" {
		return volumeRequestModeCreate
	}
	return s.Mode
}

// policy returns the deletion policy, "retain" when it is not set.
func (s *volumeRequestSpec) policy() string {
	if s.DeletionPolicy == "" {
		return volumeRequestRetain
	}
	return s.DeletionPolicy
}

// capacity returns the requested capacity in bytes.
func (s *volumeRequestSpec) capacity() (minBytes, maxBytes int64, err error) {
	parse := func(name, v string) (int64, error) {
		if v == "" {
			return 0, nil
		}
		b, err := humanize.ParseBytes(v)
		if err != nil || b > 1<<62 {
			return 0, fmt.Errorf("%s %q is not a size (for example \"10GiB\")", name, v)
		}
		return int64(b), nil
	}
	if minBytes, err = parse("capacity_min", s.CapacityMin); err != nil {
		return 0, 0, err
	}
	if maxBytes, err = parse("capacity_max", s.CapacityMax); err != nil {
		return 0, 0, err
	}
	if maxBytes > 0 && maxBytes < minBytes {
		return 0, 0, errors.New("capacity_max is less than capacity_min")
	}
	return minBytes, maxBytes, nil
}

// validate checks what the schema can't.
func (s *volumeRequestSpec) validate() error {
	switch s.mode() {
	case volumeRequestModeCreate:
		if s.ExternalID != "" {
			return errors.New("external_id is for mode \"register\": a created volume gets its ID from the plugin")
		}
		if len(s.Context) > 0 {
			return errors.New("context is for mode \"register\": a created volume gets its context from the plugin")
		}
	case volumeRequestModeRegister:
		if s.ExternalID == "" {
			return errors.New("mode \"register\" needs external_id, the ID of the volume in the storage")
		}
		if s.SnapshotID != "" || s.CloneID != "" || s.TopologyRequest != nil {
			return errors.New("snapshot_id, clone_id and topology_request are for mode \"create\"")
		}
		if s.policy() == volumeRequestDelete {
			return errors.New("deletion_policy \"delete\" is for mode \"create\": a registered volume was not made by Norca; use \"deregister\"")
		}
	}
	if s.SnapshotID != "" && s.CloneID != "" {
		return errors.New("only one of snapshot_id and clone_id")
	}
	if _, _, err := s.capacity(); err != nil {
		return err
	}
	if len(s.Capability) == 0 {
		return errors.New("at least one capability block is needed")
	}
	for _, c := range s.Capability {
		if !slices.Contains(volumeAccessModes, structs.CSIVolumeAccessMode(c.AccessMode)) {
			return fmt.Errorf("capability: unknown access_mode %q", c.AccessMode)
		}
		if !slices.Contains(volumeAttachmentModes, structs.CSIVolumeAttachmentMode(c.AttachmentMode)) {
			return fmt.Errorf("capability: unknown attachment_mode %q", c.AttachmentMode)
		}
	}
	if s.TopologyRequest != nil {
		for _, t := range append(slices.Clone(s.TopologyRequest.Required), s.TopologyRequest.Preferred...) {
			if len(t.Segments) == 0 {
				return errors.New("topology_request: a topology needs segments")
			}
		}
	}
	return nil
}

// volume returns the CSI volume the spec describes, with the secrets.
func (s *volumeRequestSpec) volume(obj *structs.CustomObject, secrets structs.CSISecrets) *structs.CSIVolume {
	minBytes, maxBytes, _ := s.capacity()
	vol := &structs.CSIVolume{
		ID:                   obj.Name,
		Name:                 s.DisplayName,
		Namespace:            obj.Namespace,
		PluginID:             s.PluginID,
		ExternalID:           s.ExternalID,
		RequestedCapacityMin: minBytes,
		RequestedCapacityMax: maxBytes,
		Secrets:              structs.CSISecrets{},
		Parameters:           maps.Clone(s.Parameters),
		Context:              maps.Clone(s.Context),
		SnapshotID:           s.SnapshotID,
		CloneID:              s.CloneID,
	}
	if vol.Name == "" {
		vol.Name = obj.Name
	}
	maps.Copy(vol.Secrets, secrets)
	if vol.Parameters == nil {
		vol.Parameters = map[string]string{}
	}
	if vol.Context == nil {
		vol.Context = map[string]string{}
	}
	for _, c := range s.Capability {
		vol.RequestedCapabilities = append(vol.RequestedCapabilities, &structs.CSIVolumeCapability{
			AccessMode:     structs.CSIVolumeAccessMode(c.AccessMode),
			AttachmentMode: structs.CSIVolumeAttachmentMode(c.AttachmentMode),
		})
	}
	if s.MountOptions != nil {
		vol.MountOptions = &structs.CSIMountOptions{FSType: s.MountOptions.FSType, MountFlags: slices.Clone(s.MountOptions.MountFlags)}
	}
	if s.TopologyRequest != nil {
		vol.RequestedTopologies = &structs.CSITopologyRequest{}
		for _, t := range s.TopologyRequest.Required {
			vol.RequestedTopologies.Required = append(vol.RequestedTopologies.Required, &structs.CSITopology{Segments: maps.Clone(t.Segments)})
		}
		for _, t := range s.TopologyRequest.Preferred {
			vol.RequestedTopologies.Preferred = append(vol.RequestedTopologies.Preferred, &structs.CSITopology{Segments: maps.Clone(t.Segments)})
		}
	}
	return vol
}

// volumeDiff returns what of the volume differs from what the spec wants,
// by the name of the field of the spec. Only what the operator sets back is
// compared: the capabilities, the mount options, the requested capacity,
// the secrets, and the context of a registered volume.
func volumeDiff(have, want *structs.CSIVolume, register bool) []string {
	var out []string
	capKey := func(caps []*structs.CSIVolumeCapability) []string {
		var keys []string
		for _, c := range caps {
			keys = append(keys, string(c.AccessMode)+"/"+string(c.AttachmentMode))
		}
		sort.Strings(keys)
		return keys
	}
	if !slices.Equal(capKey(have.RequestedCapabilities), capKey(want.RequestedCapabilities)) {
		out = append(out, "capability")
	}
	if !mountOptionsEqual(have.MountOptions, want.MountOptions) {
		out = append(out, "mount_options")
	}
	if have.RequestedCapacityMin != want.RequestedCapacityMin || have.RequestedCapacityMax != want.RequestedCapacityMax {
		out = append(out, "capacity")
	}
	if !maps.Equal(have.Secrets, want.Secrets) {
		out = append(out, "secrets")
	}
	if register && !maps.Equal(have.Context, want.Context) {
		out = append(out, "context")
	}
	return out
}

// mountOptionsEqual compares mount options; nil and empty are the same.
func mountOptionsEqual(a, b *structs.CSIMountOptions) bool {
	empty := func(o *structs.CSIMountOptions) bool {
		return o == nil || (o.FSType == "" && len(o.MountFlags) == 0 && o.MountGroup == "")
	}
	if empty(a) || empty(b) {
		return empty(a) && empty(b)
	}
	return a.FSType == b.FSType && slices.Equal(a.MountFlags, b.MountFlags) && a.MountGroup == b.MountGroup
}

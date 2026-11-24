// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"github.com/hashicorp/go-msgpack/codec"
)

// The RPC types of Rigger embed structs.WriteRequest and its kin, as those
// of the pack registry do (pack_rpc_codec.go). In the builds with the
// codegen_generated tag (the release builds) the embedded types have
// generated CodecEncodeSelf/CodecDecodeSelf methods, which an embedding type
// inherits: msgpack would encode the embedded part only. A server that was
// not the leader then forwarded a stack request without its files, and an
// answer came back with its index alone. These methods shadow the inherited
// ones and encode every field (encodeFields, decodeFields).
// TestRPCTypes_ShadowTheGeneratedCodec finds a type that has none.

func (r *RiggerBundleImportRequest) CodecEncodeSelf(e *codec.Encoder) { encodeFields(e, r) }
func (r *RiggerBundleImportRequest) CodecDecodeSelf(d *codec.Decoder) { decodeFields(d, r) }
func (r *RiggerBundleListRequest) CodecEncodeSelf(e *codec.Encoder)   { encodeFields(e, r) }
func (r *RiggerBundleListRequest) CodecDecodeSelf(d *codec.Decoder)   { decodeFields(d, r) }
func (r *RiggerBundleListResponse) CodecEncodeSelf(e *codec.Encoder)  { encodeFields(e, r) }
func (r *RiggerBundleListResponse) CodecDecodeSelf(d *codec.Decoder)  { decodeFields(d, r) }
func (r *RiggerBundleNameRequest) CodecEncodeSelf(e *codec.Encoder)   { encodeFields(e, r) }
func (r *RiggerBundleNameRequest) CodecDecodeSelf(d *codec.Decoder)   { decodeFields(d, r) }
func (r *RiggerBundleResponse) CodecEncodeSelf(e *codec.Encoder)      { encodeFields(e, r) }
func (r *RiggerBundleResponse) CodecDecodeSelf(d *codec.Decoder)      { decodeFields(d, r) }

func (r *RiggerDetailRequest) CodecEncodeSelf(e *codec.Encoder)   { encodeFields(e, r) }
func (r *RiggerDetailRequest) CodecDecodeSelf(d *codec.Decoder)   { decodeFields(d, r) }
func (r *RiggerDetailResponse) CodecEncodeSelf(e *codec.Encoder)  { encodeFields(e, r) }
func (r *RiggerDetailResponse) CodecDecodeSelf(d *codec.Decoder)  { decodeFields(d, r) }
func (r *RiggerIconResponse) CodecEncodeSelf(e *codec.Encoder)    { encodeFields(e, r) }
func (r *RiggerIconResponse) CodecDecodeSelf(d *codec.Decoder)    { decodeFields(d, r) }
func (r *RiggerInstallRequest) CodecEncodeSelf(e *codec.Encoder)  { encodeFields(e, r) }
func (r *RiggerInstallRequest) CodecDecodeSelf(d *codec.Decoder)  { decodeFields(d, r) }
func (r *RiggerInstallResponse) CodecEncodeSelf(e *codec.Encoder) { encodeFields(e, r) }
func (r *RiggerInstallResponse) CodecDecodeSelf(d *codec.Decoder) { decodeFields(d, r) }

func (r *RiggerOperationLogRequest) CodecEncodeSelf(e *codec.Encoder)  { encodeFields(e, r) }
func (r *RiggerOperationLogRequest) CodecDecodeSelf(d *codec.Decoder)  { decodeFields(d, r) }
func (r *RiggerOperationLogResponse) CodecEncodeSelf(e *codec.Encoder) { encodeFields(e, r) }
func (r *RiggerOperationLogResponse) CodecDecodeSelf(d *codec.Decoder) { decodeFields(d, r) }
func (r *RiggerOperationNameRequest) CodecEncodeSelf(e *codec.Encoder) { encodeFields(e, r) }
func (r *RiggerOperationNameRequest) CodecDecodeSelf(d *codec.Decoder) { decodeFields(d, r) }
func (r *RiggerOperationResponse) CodecEncodeSelf(e *codec.Encoder)    { encodeFields(e, r) }
func (r *RiggerOperationResponse) CodecDecodeSelf(d *codec.Decoder)    { decodeFields(d, r) }

func (r *RiggerRegistryNameRequest) CodecEncodeSelf(e *codec.Encoder)    { encodeFields(e, r) }
func (r *RiggerRegistryNameRequest) CodecDecodeSelf(d *codec.Decoder)    { decodeFields(d, r) }
func (r *RiggerRegistryResponse) CodecEncodeSelf(e *codec.Encoder)       { encodeFields(e, r) }
func (r *RiggerRegistryResponse) CodecDecodeSelf(d *codec.Decoder)       { decodeFields(d, r) }
func (r *RiggerRegistrySearchRequest) CodecEncodeSelf(e *codec.Encoder)  { encodeFields(e, r) }
func (r *RiggerRegistrySearchRequest) CodecDecodeSelf(d *codec.Decoder)  { decodeFields(d, r) }
func (r *RiggerRegistrySearchResponse) CodecEncodeSelf(e *codec.Encoder) { encodeFields(e, r) }
func (r *RiggerRegistrySearchResponse) CodecDecodeSelf(d *codec.Decoder) { decodeFields(d, r) }

func (r *RiggerStackDetailResponse) CodecEncodeSelf(e *codec.Encoder) { encodeFields(e, r) }
func (r *RiggerStackDetailResponse) CodecDecodeSelf(d *codec.Decoder) { decodeFields(d, r) }
func (r *RiggerStackPlanResponse) CodecEncodeSelf(e *codec.Encoder)   { encodeFields(e, r) }
func (r *RiggerStackPlanResponse) CodecDecodeSelf(d *codec.Decoder)   { decodeFields(d, r) }
func (r *RiggerStackRequest) CodecEncodeSelf(e *codec.Encoder)        { encodeFields(e, r) }
func (r *RiggerStackRequest) CodecDecodeSelf(d *codec.Decoder)        { decodeFields(d, r) }
func (r *RiggerStackStatusResponse) CodecEncodeSelf(e *codec.Encoder) { encodeFields(e, r) }
func (r *RiggerStackStatusResponse) CodecDecodeSelf(d *codec.Decoder) { decodeFields(d, r) }
